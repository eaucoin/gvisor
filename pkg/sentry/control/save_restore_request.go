// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package control

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/socket/unix/transport"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/syserr"
	"gvisor.dev/gvisor/pkg/waiter"
)

// saveRestoreLeaveRunning is what a save that leaves the sandbox running tells
// a save/restore socket.
const saveRestoreLeaveRunning = "leave-running"

// saveRestoreSyncs is what a save that syncs the sandbox's filesystems itself
// once its request is answered tells a save/restore socket.
const saveRestoreSyncs = "syncs"

// saveRestoreConnectRetry is how long a connection to a save/restore socket
// whose listener's backlog is full waits before it is tried again.
const saveRestoreConnectRetry = 10 * time.Millisecond

// errSaveRestoreAbandoned is the error of an exchange abandoned before it
// ended.
var errSaveRestoreAbandoned = errors.New("abandoned")

// saveRestoreRequestLine is the line a request for mode sends, telling
// detail of it if detail is not empty. detail is one line.
func saveRestoreRequestLine(mode SaveRestoreExecMode, detail string) (string, error) {
	if strings.ContainsAny(detail, "\r\n") {
		return "", fmt.Errorf("what the %s request tells must be one line, not %q", mode, detail)
	}
	if detail == "" {
		return string(mode) + "\n", nil
	}
	return string(mode) + " " + detail + "\n", nil
}

// saveRestoreAnswered is the outcome of a request whose listener answered
// answer before it ended the connection.
func saveRestoreAnswered(answer []byte) error {
	if bytes.Equal(answer, []byte("ok\n")) || (bytes.HasPrefix(answer, []byte("ok ")) && bytes.HasSuffix(answer, []byte("\n"))) {
		return nil
	}
	said := strings.TrimSpace(string(answer))
	if said == "" {
		return errors.New("the listener ended the connection without an answer")
	}
	return fmt.Errorf("the listener answered: %s", said)
}

// saveRestoreExchange sends request over ep, which is connected, and reads
// the answer until the peer ends the connection, waiting on ep's events for
// each until abandon is closed; it returns the answer's outcome.
func saveRestoreExchange(ctx context.Context, ep transport.Endpoint, request string, abandon <-chan struct{}) error {
	entry, ch := waiter.NewChannelEntry(waiter.ReadableEvents | waiter.WritableEvents | waiter.EventHUp | waiter.EventErr)
	if err := ep.EventRegister(&entry); err != nil {
		return fmt.Errorf("waiting on the connection: %w", err)
	}
	defer ep.EventUnregister(&entry)
	wait := func() error {
		select {
		case <-ch:
			return nil
		case <-abandon:
			return errSaveRestoreAbandoned
		}
	}

	for unsent := []byte(request); len(unsent) > 0; {
		n, notify, err := ep.SendMsg(ctx, [][]byte{unsent}, transport.ControlMessages{}, nil)
		if notify != nil {
			notify()
		}
		unsent = unsent[n:]
		switch {
		case err == syserr.ErrWouldBlock:
			if err := wait(); err != nil {
				return err
			}
		case err != nil:
			return fmt.Errorf("sending the request: %s", err.String())
		}
	}

	var answer []byte
	buf := make([]byte, 4096)
	for {
		out, notify, err := ep.RecvMsg(ctx, [][]byte{buf}, transport.RecvArgs{})
		if notify != nil {
			notify()
		}
		out.Control.Release(ctx)
		answer = append(answer, buf[:out.RecvLen]...)
		switch {
		case err == syserr.ErrClosedForReceive:
			return saveRestoreAnswered(answer)
		case err == syserr.ErrWouldBlock:
			if err := wait(); err != nil {
				return err
			}
		case err != nil:
			return fmt.Errorf("reading the answer: %s", err.String())
		case out.RecvLen == 0:
			return saveRestoreAnswered(answer)
		}
	}
}

// saveRestoreRequest sends mode's request, telling detail of it, to the
// save/restore socket config names, and waits for its answer within timeout,
// and loadWaitDeadline if it is not zero, as saveRestoreExec waits for a
// binary (see waitWithin).
//
// A save/restore socket is the alternative to a save/restore binary: a
// process listening on a Unix stream socket in the container, which each mode
// then costs a connection rather than a process's start. For each mode, where
// the binary would run, the Sentry connects to the socket, resolved in the
// leader task's mount namespace with its credentials, and sends one line: the
// mode, then, after a space, what it tells of it, if anything, and a newline.
// It tells a save that leaves the sandbox running "leave-running"
// (saveRestoreLeaveRunning), a save that syncs the sandbox's filesystems
// once the request is answered "syncs" (saveRestoreSyncs), after a space
// from the one before where it tells both, so that the listener need not
// sync them itself, and a restore why the sandbox stopped before it,
// where the resume that runs the restore mode was given a reason. It then
// reads the answer until the listener ends the connection. The request
// succeeds if the answer is "ok" and a newline, or "ok", a space, whatever the
// listener says of it and a newline; any other answer fails it, saying what
// the listener said. Past the wait's bound, the connection is closed and the
// request fails.
func saveRestoreRequest(k *kernel.Kernel, config *kernel.SaveRestoreExecConfig, mode SaveRestoreExecMode, detail string, timeout time.Duration, loadWaitDeadline time.Time) error {
	request, err := saveRestoreRequestLine(mode, detail)
	if err != nil {
		return err
	}
	leader := config.LeaderTask
	sctx := k.SupervisorContext()
	mntns := leader.MountNamespace()
	if mntns == nil || !mntns.TryIncRef() {
		log.Warningf("PID %d in container %q has exited, skipping the save/restore socket request for it", leader.ThreadGroup().ID(), leader.ContainerID())
		return nil
	}
	root := mntns.Root(sctx)
	ctx := vfs.WithRoot(sctx, root)
	defer func() {
		root.DecRef(sctx)
		mntns.DecRef(ctx)
	}()

	bound, err := k.VFS().BoundEndpointAt(ctx, leader.Credentials(), &vfs.PathOperation{
		Root:               root,
		Start:              root,
		Path:               fspath.Parse(config.Socket),
		FollowFinalSymlink: true,
	}, &vfs.BoundEndpointOptions{Addr: config.Socket})
	if err != nil {
		return fmt.Errorf("the save/restore socket %s: %w", config.Socket, err)
	}
	defer bound.Release(ctx)
	ep := transport.NewConnectioned(ctx, linux.SOCK_STREAM, k)

	// The exchange runs beside the wait for it, which abandons it at its
	// bound, closing the connection.
	abandon := make(chan struct{})
	done := make(chan struct{})
	var exchanged error
	start := time.Now()
	go func() {
		defer close(done)
		for {
			err := ep.Connect(ctx, bound)
			if err == nil {
				break
			}
			if err != syserr.ErrTryAgain {
				exchanged = fmt.Errorf("connecting: %s", err.String())
				return
			}
			// The listener's backlog is full: a blocking connect(2) waits.
			select {
			case <-time.After(saveRestoreConnectRetry):
			case <-abandon:
				exchanged = errSaveRestoreAbandoned
				return
			}
		}
		exchanged = saveRestoreExchange(ctx, ep, request, abandon)
	}()
	outcome := waitWithin(done, timeout, loadWaitDeadline, pgalloc.CurrentAsyncPageLoadProgress)
	if outcome != waitDone {
		close(abandon)
		<-done
	}
	ep.Close(ctx)
	switch outcome {
	case waitTimedOut:
		return fmt.Errorf("the %s request to the save/restore socket %s timed out after %v", mode, config.Socket, timeout)
	case waitLoadWaitExpired:
		return fmt.Errorf("the %s request to the save/restore socket %s waited for pages still loading past the save's load wait, abandoned: %w", mode, config.Socket, pgalloc.ErrLoadWaitExpired)
	}
	if exchanged != nil {
		return fmt.Errorf("the %s request to the save/restore socket %s failed: %w", mode, config.Socket, exchanged)
	}
	log.Infof("The %s request to the save/restore socket %s was answered in %v", mode, config.Socket, time.Since(start))
	return nil
}
