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
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/sentry/control/controlapi"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/testutil"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/socket/unix/transport"
	"gvisor.dev/gvisor/pkg/sentry/state"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/syserr"
	"gvisor.dev/gvisor/pkg/waiter"
)

// hookSocket is where the tests' listener is bound, in the leader's mount
// namespace.
const hookSocket = "/hook.sock"

// hookListener is a process's listener on a save/restore socket, as a test
// has it answer each request.
type hookListener struct {
	ctx context.Context
	ep  transport.Endpoint
	// requests receives each request's line as the listener reads it.
	requests chan string
}

// saveRestoreKernel is the tests' kernel and listener, booted once for them
// all: a second kernel's task, in the same process, waits forever for the
// ptrace platform's address space where the host has it run nested.
var saveRestoreKernel struct {
	once     sync.Once
	k        *kernel.Kernel
	listener *hookListener
	err      error
}

// newSaveRestoreKernel returns the kernel, booted at its first call, whose
// global init's leader has a tmpfs root, in which a listener is bound at
// hookSocket; and the listener. Each test configures the save/restore socket
// anew.
func newSaveRestoreKernel(t *testing.T) (*kernel.Kernel, *hookListener) {
	t.Helper()
	saveRestoreKernel.once.Do(func() {
		saveRestoreKernel.k, saveRestoreKernel.listener, saveRestoreKernel.err = bootSaveRestoreKernel()
	})
	if saveRestoreKernel.err != nil {
		t.Fatalf("booting the kernel: %v", saveRestoreKernel.err)
	}
	return saveRestoreKernel.k, saveRestoreKernel.listener
}

func bootSaveRestoreKernel() (*kernel.Kernel, *hookListener, error) {
	k, err := testutil.Boot()
	if err != nil {
		return nil, nil, fmt.Errorf("testutil.Boot() failed: %w", err)
	}
	ctx := k.SupervisorContext()
	creds := auth.CredentialsFromContext(ctx)
	mntns, err := k.VFS().NewMountNamespace(ctx, creds, "", "tmpfs", &vfs.MountOptions{}, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("NewMountNamespace() failed: %w", err)
	}
	root := mntns.Root(ctx)
	defer root.DecRef(ctx)
	if _, err := testutil.CreateTask(ctx, "leader", k.GlobalInit(), mntns, root, root); err != nil {
		return nil, nil, fmt.Errorf("CreateTask() failed: %w", err)
	}

	ep := transport.NewConnectioned(ctx, linux.SOCK_STREAM, k)
	if err := ep.Bind(transport.Address{Addr: hookSocket}); err != nil {
		return nil, nil, fmt.Errorf("Bind() failed: %s", err.String())
	}
	if err := ep.Listen(ctx, 8); err != nil {
		return nil, nil, fmt.Errorf("Listen() failed: %s", err.String())
	}
	if err := k.VFS().MknodAt(ctx, creds, &vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse(hookSocket),
	}, &vfs.MknodOptions{Mode: linux.S_IFSOCK | 0600, Endpoint: ep.(transport.BoundEndpoint)}); err != nil {
		return nil, nil, fmt.Errorf("MknodAt() failed: %w", err)
	}
	return k, &hookListener{ctx: ctx, ep: ep, requests: make(chan string, 16)}, nil
}

// waitOn waits for one of mask's events on ep.
func waitOn(ep transport.Endpoint, mask waiter.EventMask) {
	entry, ch := waiter.NewChannelEntry(mask)
	if err := ep.EventRegister(&entry); err != nil {
		panic(err)
	}
	defer ep.EventUnregister(&entry)
	if ep.Readiness(mask) == 0 {
		<-ch
	}
}

// serve has the listener take one connection, read its request's line, wait
// for delay, then answer answer and end the connection. An empty answer is
// none; held, the connection is not ended until the test is.
func (l *hookListener) serve(t *testing.T, answer string, delay time.Duration, held bool) {
	t.Helper()
	go func() {
		var conn transport.Endpoint
		for {
			c, err := l.ep.Accept(l.ctx, nil)
			if err == nil {
				conn = c
				break
			}
			if err != syserr.ErrWouldBlock {
				t.Errorf("Accept() failed: %v", err)
				return
			}
			waitOn(l.ep, waiter.ReadableEvents)
		}
		var line []byte
		buf := make([]byte, 64)
		for !bytes.HasSuffix(line, []byte("\n")) {
			out, _, err := conn.RecvMsg(l.ctx, [][]byte{buf}, transport.RecvArgs{})
			line = append(line, buf[:out.RecvLen]...)
			if err == syserr.ErrWouldBlock {
				waitOn(conn, waiter.ReadableEvents|waiter.EventHUp)
			} else if err != nil {
				t.Errorf("RecvMsg() failed: %v", err)
				return
			}
		}
		l.requests <- string(line)
		time.Sleep(delay)
		if held {
			t.Cleanup(func() { conn.Close(l.ctx) })
			return
		}
		if answer != "" {
			if _, notify, err := conn.SendMsg(l.ctx, [][]byte{[]byte(answer)}, transport.ControlMessages{}, nil); err != nil {
				t.Errorf("SendMsg() failed: %v", err)
			} else if notify != nil {
				notify()
			}
		}
		conn.Close(l.ctx)
	}()
}

// request is the line the listener read of the request it took.
func (l *hookListener) request(t *testing.T) string {
	t.Helper()
	select {
	case line := <-l.requests:
		return line
	case <-time.After(10 * time.Second):
		t.Fatalf("the listener read no request")
		return ""
	}
}

func TestSaveRestoreSocketModes(t *testing.T) {
	k, l := newSaveRestoreKernel(t)
	execOpts := &controlapi.SaveRestoreExecOpts{Socket: hookSocket, Timeout: time.Minute}

	// A save that leaves the sandbox running says so.
	l.serve(t, "ok\n", 0, false)
	if err := preSave(k, &state.SaveOpts{Resume: true}, execOpts); err != nil {
		t.Fatalf("preSave() failed: %v", err)
	}
	if got, want := l.request(t), "save leave-running\n"; got != want {
		t.Errorf("save request: got %q, want %q", got, want)
	}
	if k.SaveRestoreExecConfig == nil || k.SaveRestoreExecConfig.Socket != hookSocket || len(k.SaveRestoreExecConfig.Argv) != 0 {
		t.Errorf("configuration after the save: got %+v, want the socket %q and no argv", k.SaveRestoreExecConfig, hookSocket)
	}

	// What the listener says after "ok " is its own.
	l.serve(t, "ok {\"caught\":1}\n", 0, false)
	if err := SaveRestoreExec(k, SaveRestoreExecResume); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got, want := l.request(t), "resume\n"; got != want {
		t.Errorf("resume request: got %q, want %q", got, want)
	}

	// A save that does not leave it running tells nothing.
	l.serve(t, "ok\n", 0, false)
	if err := preSave(k, &state.SaveOpts{}, execOpts); err != nil {
		t.Fatalf("preSave() failed: %v", err)
	}
	if got, want := l.request(t), "save\n"; got != want {
		t.Errorf("save request: got %q, want %q", got, want)
	}

	// A restore tells why the sandbox stopped, where it was told.
	for _, tc := range []struct {
		reason string
		want   string
	}{
		{reason: "", want: "restore\n"},
		{reason: "it ran out of its memory (1Gi)", want: "restore it ran out of its memory (1Gi)\n"},
	} {
		l.serve(t, "ok\n", 0, false)
		if err := PostRestore(k, nil, tc.reason); err != nil {
			t.Fatalf("PostRestore(%q) failed: %v", tc.reason, err)
		}
		if got := l.request(t); got != tc.want {
			t.Errorf("restore request told %q: got %q, want %q", tc.reason, got, tc.want)
		}
	}
}

func TestSaveRestoreSocketFailures(t *testing.T) {
	k, l := newSaveRestoreKernel(t)
	if err := ConfigureSaveRestoreExec(k, nil, hookSocket, time.Minute, 0, ""); err != nil {
		t.Fatalf("ConfigureSaveRestoreExec() failed: %v", err)
	}
	for _, tc := range []struct {
		name   string
		answer string
		want   string
	}{
		{name: "refused", answer: "could not save: the disk is full\n", want: "the listener answered: could not save: the disk is full"},
		{name: "unanswered", answer: "", want: "the listener ended the connection without an answer"},
		{name: "unended", answer: "ok", want: "the listener answered: ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l.serve(t, tc.answer, 0, false)
			err := SaveRestoreExec(k, SaveRestoreExecSave)
			l.request(t)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("SaveRestoreExec() = %v, want an error saying %q", err, tc.want)
			}
		})
	}

	// A detail of more than one line is not sent.
	if err := PostRestore(k, nil, "one\nmore"); err == nil || !strings.Contains(err.Error(), "must be one line") {
		t.Errorf("PostRestore() told two lines = %v, want an error saying it must be one line", err)
	}
	// Nothing listens there.
	if err := ConfigureSaveRestoreExec(k, nil, "/none.sock", time.Minute, 0, ""); err != nil {
		t.Fatalf("ConfigureSaveRestoreExec() failed: %v", err)
	}
	if err := SaveRestoreExec(k, SaveRestoreExecSave); err == nil || !strings.Contains(err.Error(), "/none.sock") {
		t.Errorf("SaveRestoreExec() to no socket = %v, want an error naming it", err)
	}
	// A binary and a socket are not both run.
	if err := preSave(k, &state.SaveOpts{}, &controlapi.SaveRestoreExecOpts{Argv: "/bin/true", Socket: hookSocket}); err == nil || !strings.Contains(err.Error(), "give one") {
		t.Errorf("preSave() with a binary and a socket = %v, want an error", err)
	}
}

// stubSync replaces syncSandboxFilesystems, for the test, with a sync that
// fails with err, if it is not nil, and returns how many syncs ran, each once
// the listener had read the save request that came before it.
func stubSync(t *testing.T, l *hookListener, err error) *int {
	t.Helper()
	synced := new(int)
	was := syncSandboxFilesystems
	t.Cleanup(func() { syncSandboxFilesystems = was })
	syncSandboxFilesystems = func(*kernel.Kernel) error {
		if got := len(l.requests); got != 1 {
			t.Errorf("a sync ran with %d requests read before it, want it after the save request alone", got)
		}
		*synced++
		return err
	}
	return synced
}

func TestSaveRestoreSocketSync(t *testing.T) {
	k, l := newSaveRestoreKernel(t)
	synced := stubSync(t, l, nil)
	for _, tc := range []struct {
		name   string
		resume bool
		want   string
	}{
		{name: "leaving it running", resume: true, want: "save leave-running syncs\n"},
		{name: "ending it", resume: false, want: "save syncs\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := *synced
			l.serve(t, "ok\n", 0, false)
			if err := preSave(k, &state.SaveOpts{Resume: tc.resume}, &controlapi.SaveRestoreExecOpts{Socket: hookSocket, Timeout: time.Minute, Sync: true}); err != nil {
				t.Fatalf("preSave() failed: %v", err)
			}
			if got := l.request(t); got != tc.want {
				t.Errorf("save request: got %q, want %q", got, tc.want)
			}
			if got := *synced - before; got != 1 {
				t.Errorf("syncs after the save request: got %d, want 1", got)
			}
		})
	}

	// Without Sync, the save syncs nothing, and tells nothing of it.
	before := *synced
	l.serve(t, "ok\n", 0, false)
	if err := preSave(k, &state.SaveOpts{}, &controlapi.SaveRestoreExecOpts{Socket: hookSocket, Timeout: time.Minute}); err != nil {
		t.Fatalf("preSave() failed: %v", err)
	}
	if got, want := l.request(t), "save\n"; got != want {
		t.Errorf("save request: got %q, want %q", got, want)
	}
	if got := *synced - before; got != 0 {
		t.Errorf("syncs of a save without Sync: got %d, want 0", got)
	}

	// A save that is refused syncs nothing.
	l.serve(t, "could not save\n", 0, false)
	if err := preSave(k, &state.SaveOpts{}, &controlapi.SaveRestoreExecOpts{Socket: hookSocket, Timeout: time.Minute, Sync: true}); err == nil {
		t.Errorf("preSave() of a refused save succeeded")
	}
	l.request(t)
	if got := *synced - before; got != 0 {
		t.Errorf("syncs of a refused save: got %d, want 0", got)
	}

	// Only a socket is told that the save syncs.
	if err := preSave(k, &state.SaveOpts{}, &controlapi.SaveRestoreExecOpts{Argv: "/bin/true", Timeout: time.Minute, Sync: true}); err == nil || !strings.Contains(err.Error(), "give one, not a save/restore binary") {
		t.Errorf("preSave() syncing with a binary = %v, want an error", err)
	}
}

func TestSaveRestoreSocketSyncFailure(t *testing.T) {
	k, l := newSaveRestoreKernel(t)
	synced := stubSync(t, l, errors.New("the gofer is gone"))
	l.serve(t, "ok\n", 0, false)
	resume, err := SaveWithOpts(k, nil, &state.SaveOpts{Resume: true}, &controlapi.SaveRestoreExecOpts{Socket: hookSocket, Timeout: time.Minute, Sync: true})
	if got, want := l.request(t), "save leave-running syncs\n"; got != want {
		t.Errorf("save request: got %q, want %q", got, want)
	}
	if *synced != 1 {
		t.Errorf("syncs: got %d, want 1", *synced)
	}
	if err == nil || !errors.Is(err, errSaveSyncFailed) || !strings.Contains(err.Error(), "the gofer is gone") {
		t.Fatalf("SaveWithOpts() with a sync that fails = %v, want the sync's error", err)
	}
	// The sandbox runs on: the socket, asked the save, is told of its resume.
	if resume == nil {
		t.Fatalf("SaveWithOpts() with a sync that fails returned no resume, so the socket asked the save is never told the sandbox runs on")
	}
	l.serve(t, "ok\n", 0, false)
	if err := resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got, want := l.request(t), "resume\n"; got != want {
		t.Errorf("request after the failed save: got %q, want %q", got, want)
	}
}

func TestSaveRestoreSocketTimeouts(t *testing.T) {
	k, l := newSaveRestoreKernel(t)
	// The save and resume modes are bounded by the save timeout, the restore
	// mode by the timeout.
	if err := ConfigureSaveRestoreExec(k, nil, hookSocket, time.Minute, 200*time.Millisecond, ""); err != nil {
		t.Fatalf("ConfigureSaveRestoreExec() failed: %v", err)
	}
	for _, mode := range []SaveRestoreExecMode{SaveRestoreExecSave, SaveRestoreExecResume} {
		l.serve(t, "", 0, true)
		start := time.Now()
		err := SaveRestoreExec(k, mode)
		if err == nil || !strings.Contains(err.Error(), "timed out after 200ms") {
			t.Errorf("%s unanswered: got %v, want it timed out after 200ms", mode, err)
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("%s unanswered took %v, past its bound", mode, took)
		}
		l.request(t)
	}
	l.serve(t, "ok\n", time.Second, false)
	if err := PostRestore(k, nil, ""); err != nil {
		t.Errorf("a restore answered after a second, its bound a minute: %v", err)
	}
	l.request(t)

	// Without a save timeout, the timeout bounds every mode.
	if err := ConfigureSaveRestoreExec(k, nil, hookSocket, 200*time.Millisecond, 0, ""); err != nil {
		t.Fatalf("ConfigureSaveRestoreExec() failed: %v", err)
	}
	l.serve(t, "", 0, true)
	if err := PostRestore(k, nil, ""); err == nil || !strings.Contains(err.Error(), "timed out after 200ms") {
		t.Errorf("restore unanswered: got %v, want it timed out after 200ms", err)
	}
	l.request(t)
}
