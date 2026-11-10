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

package runsc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	task "github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/log"
	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/shim/v1/extension"
	"gvisor.dev/gvisor/pkg/shim/v1/proc"
	"gvisor.dev/gvisor/pkg/shim/v1/utils"
	"gvisor.dev/gvisor/runsc/specutils"
)

// A standby sandbox is a pod whose sandbox has booted and whose containers
// runsc has created, but which runs nothing: the shim reports its containers
// started, so that the pod is running, with its address and its network
// policies in place, before any checkpoint is restored into it. Its shim then
// takes one request, on the socket that restoreStandbyAnnotation names, to
// restore a checkpoint into the sandbox, as a pod created with the restore
// annotations is restored when its containers start: runsc restores each of
// its containers, in the order they were created, from the checkpoint. The
// request is
//
//	POST /restore
//	{"imagePath": "...", "layerPaths": [...], "direct": false, "background": false, "paused": true}
//
// and the answer, once every container is restored, 200 with the time each
// restore took, or an error, after which the sandbox is stopped and its
// containers exit. A standby is single-use: its socket takes one request. A
// container created in the sandbox once that request was made is no
// standby's: it starts as any container does.
//
// Where each container has a shim of its own (shim grouping off), the shim of
// the sandbox's root container serves the socket the annotation names and
// restores the root container alone; the shim of each other container serves
// that container's own socket (containerStandbySocket), and the client posts
// the same request to each, the root's first.

// standbySocketDir is the directory that the socket of every standby is in,
// which only the shim's user may write to.
const standbySocketDir = "/run/gvisor/standby"

// standbySocketName is what the name of a standby's socket in
// standbySocketDir is made of.
var standbySocketName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*\.sock$`)

// standbyContainerName is what the name of a container of a standby, which
// its own socket is named after (containerStandbySocket), is made of.
var standbyContainerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// validStandbySocket returns an error unless socket is a socket's path in
// standbySocketDir, and clean: filepath.Dir would resolve a ".." in it
// lexically, as if no component before it were a symbolic link.
func validStandbySocket(socket string) error {
	if filepath.Clean(socket) != socket || filepath.Dir(socket) != standbySocketDir || !standbySocketName.MatchString(filepath.Base(socket)) {
		return fmt.Errorf("standby socket %q is not a %s/<name>.sock, its name of letters, digits, '_', '.' and '-'", socket, standbySocketDir)
	}
	return nil
}

// standbyRestoreRequest is a request to restore a checkpoint into a standby.
type standbyRestoreRequest struct {
	ImagePath  string   `json:"imagePath"`
	LayerPaths []string `json:"layerPaths"`
	Direct     bool     `json:"direct"`
	Background bool     `json:"background"`
	Paused     bool     `json:"paused"`
}

// standbyRestored is the answer to a restore into a standby.
type standbyRestored struct {
	// Containers are the restored containers' ids, in the order restored.
	Containers []string `json:"containers"`
	// Milliseconds are the times each container's restore took.
	Milliseconds []float64 `json:"milliseconds"`
}

// standbySandbox is the process of a standby's sandbox, held by a pidfd that
// was opened as a container of the standby was created, through which the
// shim watches and kills it: its PID alone could name another process once
// the sandbox's is reaped.
type standbySandbox struct {
	pid int
	// start is the time the process started, in clock ticks after boot,
	// which /proc/pid/stat tells it from another process given its PID by.
	start uint64

	mu sync.Mutex
	// pidfd is the process's pidfd; -1 once closed.
	//
	// +checklocks:mu
	pidfd int
}

// openStandbySandbox returns the sandbox process pid, as runsc create named
// it.
func openStandbySandbox(pid int) (*standbySandbox, error) {
	_, start, err := processStat(pid)
	if err != nil {
		return nil, fmt.Errorf("standby sandbox (PID %d): %w", pid, err)
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, fmt.Errorf("standby sandbox (PID %d): pidfd_open: %w", pid, err)
	}
	// A process that started at another time took the PID after the
	// sandbox's was reaped, between the two.
	if _, again, err := processStat(pid); err == nil && again != start {
		unix.Close(pidfd)
		return nil, fmt.Errorf("standby sandbox (PID %d) is gone, its PID another process's", pid)
	}
	return &standbySandbox{pid: pid, start: start, pidfd: pidfd}, nil
}

// kill kills the sandbox's process, if it is not reaped.
func (sb *standbySandbox) kill() error {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.pidfd < 0 {
		return fmt.Errorf("pidfd of standby sandbox (PID %d): %w", sb.pid, os.ErrClosed)
	}
	if err := unix.PidfdSendSignal(sb.pidfd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("killing standby sandbox (PID %d): %w", sb.pid, err)
	}
	return nil
}

// dup returns a pidfd of the sandbox's process of the caller's own, which it
// closes.
func (sb *standbySandbox) dup() (int, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.pidfd < 0 {
		return -1, fmt.Errorf("pidfd of standby sandbox (PID %d): %w", sb.pid, os.ErrClosed)
	}
	pidfd, err := unix.FcntlInt(uintptr(sb.pidfd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("pidfd of standby sandbox (PID %d): %w", sb.pid, err)
	}
	return pidfd, nil
}

// close closes the pidfd; nothing where sb is nil.
func (sb *standbySandbox) close() {
	if sb == nil {
		return
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.pidfd >= 0 {
		unix.Close(sb.pidfd)
		sb.pidfd = -1
	}
}

// alive returns the state of the sandbox's process, and whether it is alive:
// neither a zombie nor gone, nor another process given its PID.
func (sb *standbySandbox) alive() (string, bool) {
	state, start, err := processStat(sb.pid)
	switch {
	case err != nil:
		return err.Error(), false
	case start != sb.start:
		return fmt.Sprintf("its PID that of a process started at %d, not %d", start, sb.start), false
	default:
		return fmt.Sprintf("state %c", state), state != 'Z' && state != 'X'
	}
}

// processStat returns the state of the process pid and the time it started,
// as /proc/pid/stat says them.
func processStat(pid int) (byte, uint64, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, err
	}
	// The fields after the command, which is in parentheses, are the state,
	// the third field, and, 19 fields later, the start time, the 22nd.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, 0, fmt.Errorf("/proc/%d/stat: %q", pid, stat)
	}
	fields := strings.Fields(string(stat[i+1:]))
	if len(fields) < 20 || len(fields[0]) != 1 {
		return 0, 0, fmt.Errorf("/proc/%d/stat: %q", pid, stat)
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("/proc/%d/stat: start time: %w", pid, err)
	}
	return fields[0][0], start, nil
}

// standbyServer serves the restore request of a standby sandbox, and reports
// its containers' exits while none was made, as nothing else waits on them.
type standbyServer struct {
	s       *runscService
	socket  string
	sandbox *standbySandbox

	// stopWatch, closed, stops watch.
	stopWatch     chan struct{}
	stopWatchOnce sync.Once

	mu sync.Mutex
	// listener is nil until the socket is listened on.
	//
	// +checklocks:mu
	listener net.Listener
	// watched is the pidfd that watch polls, which it closes; -1 once watch
	// started.
	//
	// +checklocks:mu
	watched int
	// requested is true once a restore was requested.
	//
	// +checklocks:mu
	requested bool
	// restoring are the containers that the restore requested restores, those
	// created before it was: no other container of the sandbox is a
	// standby's.
	//
	// +checklocks:mu
	restoring []*Container
	// +checklocks:mu
	closed bool
}

// closeStandbyStdio is the function that standbyExited and Delete close the
// I/O of a standby's container with: closeStdio, which the package's tests
// replace, before any of them runs, to record the containers whose I/O is
// closed.
var closeStandbyStdio = closeStdio

// closeStdio closes the I/O the shim holds for c's init process
// (proc.Init.CloseStdio). A standby's container that is not its sandbox's
// own was never started, which would have given runsc the pipes the shim made
// for it: the shim holds both ends of each, and its copies from them into the
// container's FIFOs end only as they are closed. The sandbox's own container
// gave its pipes to runsc create, and their copies end as the sandbox exits;
// closing them ends them at once.
func closeStdio(c *Container) {
	if p, err := c.runscInit(); err == nil {
		p.CloseStdio()
	}
}

// standbyExited reports the exit of c, a standby's container that no restore
// was made into, at status, its I/O closed first (closeStandbyStdio). It
// returns false, and does nothing, where c is not a standby's, or no more.
//
// +checklocksexclude:s.mu
func (s *runscService) standbyExited(c *Container, status int) bool {
	if c.takeStandby() == nil {
		return false
	}
	closeStandbyStdio(c)
	s.ec <- proc.Exit{Timestamp: time.Now(), ID: c.ID, Status: status}
	return true
}

// containerStandbySocket is the socket of the standby's container named name,
// served by the shim of that container alone, as each container has a shim of
// its own where shim grouping is off: the sandbox's socket, socket, with the
// container's name before its extension.
func containerStandbySocket(socket, name string) (string, error) {
	if !standbyContainerName.MatchString(name) {
		return "", fmt.Errorf("container name %q of a standby is not one of letters, digits, '_', '.' and '-'", name)
	}
	return strings.TrimSuffix(socket, ".sock") + "." + name + ".sock", nil
}

// serveStandby prepares the standby's server on socket, for the sandbox of c,
// a standby's container: the sandbox's socket, served by the shim of its root
// container, which restores every container of the sandbox this shim created;
// or a container's socket (containerStandbySocket), served by the shim of that
// container alone, where shim grouping is off. Its run starts it.
//
// +checklocksexclude:s.mu
func (s *runscService) serveStandby(c *Container, socket string) (*standbyServer, error) {
	if c.standbySandbox == nil {
		return nil, fmt.Errorf("container %q is not a standby's", c.ID)
	}
	srv := &standbyServer{
		s:         s,
		socket:    socket,
		sandbox:   c.standbySandbox,
		stopWatch: make(chan struct{}),
		watched:   -1,
	}
	// The server is the shim's before the socket is touched, so that no other
	// takes its place meanwhile.
	s.mu.Lock()
	if s.standby != nil {
		served := s.standby.socket
		s.mu.Unlock()
		return nil, fmt.Errorf("standby sandbox already served on %q", served)
	}
	s.standby = srv
	s.mu.Unlock()
	if err := srv.listen(); err != nil {
		s.mu.Lock()
		if s.standby == srv {
			s.standby = nil
		}
		s.mu.Unlock()
		return nil, err
	}
	return srv, nil
}

// listen listens on the server's socket, in a directory of the shim's user
// that only it may write to, in place of nothing or of a socket that no one
// listens on, and readies the pidfd that watch polls.
func (srv *standbyServer) listen() error {
	dir := filepath.Dir(srv.socket)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("standby socket directory: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("standby socket directory: %w", err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || !fi.IsDir() || fi.Mode().Perm()&0022 != 0 || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("standby socket directory %q (%v) is not a directory of the shim's user that only it may write to", dir, fi.Mode())
	}
	switch fi, err := os.Lstat(srv.socket); {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("standby socket: %w", err)
	case fi.Mode().Type() != fs.ModeSocket:
		return fmt.Errorf("standby socket %q: a %v is in its place, not a socket", srv.socket, fi.Mode().Type())
	default:
		if conn, err := net.DialTimeout("unix", srv.socket, time.Second); err == nil {
			conn.Close()
			return fmt.Errorf("standby socket %q is listened on already", srv.socket)
		}
		// A socket that a shim before this one left.
		if err := os.Remove(srv.socket); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("standby socket: %w", err)
		}
	}
	listener, err := net.Listen("unix", srv.socket)
	if err != nil {
		return fmt.Errorf("standby socket: %w", err)
	}
	if err := os.Chmod(srv.socket, 0600); err != nil {
		listener.Close()
		return fmt.Errorf("standby socket: %w", err)
	}
	watched, err := srv.sandbox.dup()
	if err != nil {
		listener.Close()
		return err
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.closed {
		// Closed as its container was deleted.
		listener.Close()
		unix.Close(watched)
		return fmt.Errorf("standby socket %q: closed as it was opened", srv.socket)
	}
	srv.listener = listener
	srv.watched = watched
	return nil
}

// run starts serving the socket, and watching the sandbox: once the start of
// its container is reported, so that no exit of it is reported before.
func (srv *standbyServer) run() {
	srv.mu.Lock()
	listener, watched := srv.listener, srv.watched
	srv.watched = -1
	srv.mu.Unlock()
	if watched >= 0 {
		go srv.watch(watched)
	}
	if listener != nil {
		go func() {
			if err := http.Serve(listener, srv); err != nil && !errors.Is(err, net.ErrClosed) {
				log.L.Warningf("Standby socket %q: %v", srv.socket, err)
			}
		}()
	}
	log.L.Infof("Standby sandbox (PID %d) takes a restore on %q", srv.sandbox.pid, srv.socket)
}

// stopWatching stops watch.
func (srv *standbyServer) stopWatching() {
	srv.stopWatchOnce.Do(func() { close(srv.stopWatch) })
}

// close stops serving the socket, and watching the sandbox, and removes the
// socket.
func (srv *standbyServer) close() {
	srv.mu.Lock()
	if srv.closed {
		srv.mu.Unlock()
		return
	}
	srv.closed = true
	listener, watched := srv.listener, srv.watched
	srv.watched = -1
	srv.mu.Unlock()
	srv.stopWatching()
	if watched >= 0 {
		unix.Close(watched)
	}
	if listener != nil {
		listener.Close()
		os.Remove(srv.socket)
	}
}

// over returns true if no restore into the standby will be made into c, a
// standby's container that is not its sandbox's own: a restore of the
// containers created before c was requested, or the sandbox exited.
func (srv *standbyServer) over(c *Container) bool {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.requested {
		for _, restoring := range srv.restoring {
			if restoring == c {
				return false
			}
		}
		return true
	}
	return srv.closed
}

// ServeHTTP implements http.Handler.ServeHTTP for the restore request.
//
// +checklocksexclude:srv.s.mu
func (srv *standbyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/restore" {
		http.Error(w, "only POST /restore", http.StatusNotFound)
		return
	}
	var req standbyRestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("restore request: %v", err), http.StatusBadRequest)
		return
	}
	if req.ImagePath == "" {
		http.Error(w, "restore request without imagePath", http.StatusBadRequest)
		return
	}
	containers := srv.s.createdContainers()
	srv.mu.Lock()
	requested := srv.requested
	if !requested {
		srv.requested = true
		srv.restoring = containers
	}
	srv.mu.Unlock()
	if requested {
		http.Error(w, "a restore was requested of this standby already", http.StatusConflict)
		return
	}
	defer srv.close()
	// A client gone does not stop the restore: its containers would be left
	// half restored.
	restored, err := srv.restore(context.WithoutCancel(r.Context()), &req, containers)
	if err != nil {
		log.L.Warningf("Restore into standby sandbox (PID %d) failed: %v", srv.sandbox.pid, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(restored); err != nil {
		log.L.Warningf("Answering the restore into standby sandbox (PID %d): %v", srv.sandbox.pid, err)
	}
}

// restore restores the checkpoint req names into every container of the
// standby, containers, in the order they were created. Every one must be a
// standby's still: one killed or deleted since fails the restore, as the
// sandbox would be restored without it.
//
// +checklocksexclude:srv.s.mu
func (srv *standbyServer) restore(ctx context.Context, req *standbyRestoreRequest, containers []*Container) (*standbyRestored, error) {
	// From here on, the restored containers' runsc wait reports their exits.
	srv.stopWatching()
	if fi, err := os.Stat(req.ImagePath); err != nil || !fi.IsDir() {
		srv.fail(nil)
		return nil, fmt.Errorf("checkpoint %q is not a directory: %v", req.ImagePath, err)
	}
	var restored standbyRestored
	for _, c := range containers {
		conf := c.takeStandby()
		if conf == nil {
			srv.fail(nil)
			return nil, fmt.Errorf("container %q of the standby was killed or deleted as the restore was requested: not restored", c.ID)
		}
		conf.ImagePath = req.ImagePath
		conf.LayerPaths = req.LayerPaths
		conf.Direct = conf.Direct || req.Direct
		conf.Background = conf.Background || req.Background
		conf.Paused = req.Paused
		start := time.Now()
		if _, err := c.Restore(ctx, &extension.RestoreRequest{Start: &task.StartRequest{ID: c.ID}, Conf: *conf}); err != nil {
			srv.fail(c)
			return nil, fmt.Errorf("restoring container %q: %w", c.ID, err)
		}
		elapsed := time.Since(start)
		log.L.Infof("Restored container %q into standby sandbox in %v", c.ID, elapsed)
		restored.Containers = append(restored.Containers, c.ID)
		restored.Milliseconds = append(restored.Milliseconds, float64(elapsed.Microseconds())/1000)
	}
	return &restored, nil
}

// fail stops the sandbox after a restore into it failed, at failed, the
// container whose restore failed (nil if none was attempted), and reports the
// exits of failed and of every container not attempted: those restored before
// it exit as the sandbox stops, through their runsc wait.
//
// +checklocksexclude:srv.s.mu
func (srv *standbyServer) fail(failed *Container) {
	if err := srv.sandbox.kill(); err != nil {
		log.L.Warningf("Stopping the standby sandbox whose restore failed: %v", err)
	}
	if failed != nil {
		// Its failed restore stopped it, its I/O closed (createdState.Start).
		srv.s.ec <- proc.Exit{Timestamp: time.Now(), ID: failed.ID, Status: proc.InternalErrorCode}
	}
	for _, c := range srv.s.createdContainers() {
		srv.s.standbyExited(c, proc.InternalErrorCode)
	}
}

// watch reports the exit of every container of the standby that is one still
// once its sandbox's process exits, before its container started or after,
// until stopWatch is closed. It polls pidfd, which it closes.
//
// +checklocksexclude:srv.s.mu
func (srv *standbyServer) watch(pidfd int) {
	defer unix.Close(pidfd)
	pid := srv.sandbox.pid
	for {
		select {
		case <-srv.stopWatch:
			return
		default:
		}
		fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 100 /* ms */)
		if errors.Is(err, unix.EINTR) || (err == nil && n == 0) {
			continue
		}
		if err != nil {
			log.L.Warningf("Watching standby sandbox (PID %d): %v", pid, err)
			return
		}
		select {
		case <-srv.stopWatch:
			// Stopped by a restore, which may have stopped the sandbox since:
			// the restore reports the exits.
			return
		default:
		}
		state, alive := srv.sandbox.alive()
		if alive {
			// Seen while the sandbox execs itself early in its boot, from a
			// thread other than its leader: keep watching.
			log.L.Warningf("Standby sandbox (PID %d) polled exited (events %#x) but is alive (%s)", pid, fds[0].Revents, state)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		log.L.Infof("Standby sandbox (PID %d) exited before a restore into it (events %#x, %s)", pid, fds[0].Revents, state)
		srv.close()
		for _, c := range srv.s.createdContainers() {
			srv.s.standbyExited(c, 128+int(unix.SIGKILL))
		}
		return
	}
}

// killStandby kills the container c of a standby, which runs nothing: the
// sandbox, if c is its root container, whose exit watch reports; c alone
// otherwise, whose exit is reported at once. It returns false, and does
// nothing, where c is not a standby's, or no more, as a restore took it.
//
// +checklocksexclude:s.mu
func (s *runscService) killStandby(c *Container, signal uint32) bool {
	p, err := c.runscInit()
	if err != nil || !c.isStandby() {
		return false
	}
	log.L.Infof("Signal %d to container %q of a standby sandbox, which runs nothing", signal, c.ID)
	if p.Sandbox {
		if err := c.standbySandbox.kill(); err != nil {
			log.L.Warningf("Killing the standby sandbox of container %q: %v", c.ID, err)
		}
		return true
	}
	return s.standbyExited(c, 128+int(signal))
}

// standbyOver returns true if c, a standby's container that is not its
// sandbox's own, was created once no restore into the standby would be made
// into it: once the restore of the containers created before it was
// requested, or the sandbox exited, as where kubelet created it again after
// it exited. It then starts as any container does.
//
// +checklocksexclude:s.mu
func (s *runscService) standbyOver(ctx context.Context, c *Container) bool {
	p, err := c.runscInit()
	if err != nil || p.Sandbox {
		return false
	}
	s.mu.Lock()
	srv, name := s.standby, s.standbyNames[c.ID]
	s.mu.Unlock()
	if srv != nil {
		return srv.over(c)
	}
	if name == "" {
		return false
	}
	// This shim holds c alone, shim grouping off: runsc keeps the sandbox's
	// root container created until a restore is made into it.
	spec, err := utils.ReadSpec(c.Bundle)
	if err != nil {
		log.L.Warningf("Standby container %q: %v", c.ID, err)
		return false
	}
	root, ok := specutils.SandboxID(spec)
	if !ok {
		return false
	}
	state, err := p.Runtime().State(ctx, root)
	if err != nil {
		log.L.Warningf("Standby container %q: state of its sandbox's root container %q: %v", c.ID, root, err)
		return false
	}
	return state.Status != "created"
}

// createdContainers returns the containers, in the order they were created.
//
// +checklocksexclude:s.mu
func (s *runscService) createdContainers() []*Container {
	s.mu.Lock()
	defer s.mu.Unlock()
	containers := make([]*Container, 0, len(s.created))
	for _, id := range s.created {
		containers = append(containers, s.containers[id])
	}
	return containers
}
