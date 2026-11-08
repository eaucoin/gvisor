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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	task "github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/log"
	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/shim/v1/extension"
	"gvisor.dev/gvisor/pkg/shim/v1/proc"
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
// containers exit. A standby is single-use: its socket takes one request.
//
// Where each container has a shim of its own (shim grouping off), the shim of
// the sandbox's root container serves the socket the annotation names and
// restores the root container alone; the shim of each other container serves
// that container's own socket (containerStandbySocket), and the client posts
// the same request to each, the root's first.

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

// standbyServer serves the restore request of a standby sandbox, and reports
// its containers' exits while none was made, as nothing else waits on them.
type standbyServer struct {
	s        *runscService
	socket   string
	listener net.Listener
	// pid is the sandbox's process.
	pid int

	// stopWatch, closed, stops watch.
	stopWatch chan struct{}

	mu sync.Mutex
	// +checklocks:mu
	requested bool
	// +checklocks:mu
	closed bool
}

// containerStandbySocket is the socket of the standby's container named name,
// served by the shim of that container alone, as each container has a shim of
// its own where shim grouping is off: the sandbox's socket, socket, with the
// container's name before its extension.
func containerStandbySocket(socket, name string) string {
	return strings.TrimSuffix(socket, ".sock") + "." + name + ".sock"
}

// serveStandby starts the standby's server on socket, for its sandbox, whose
// process is pid: the sandbox's socket, served by the shim of its root
// container, which restores every container of the sandbox this shim
// created; or a container's socket (containerStandbySocket), served by the
// shim of that container alone, where shim grouping is off.
//
// +checklocksexclude:s.mu
func (s *runscService) serveStandby(pid int, socket string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.standby != nil {
		return fmt.Errorf("standby sandbox already served on %q", s.standby.socket)
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return fmt.Errorf("standby socket directory: %w", err)
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("standby socket: %w", err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("standby socket: %w", err)
	}
	if err := os.Chmod(socket, 0600); err != nil {
		listener.Close()
		return fmt.Errorf("standby socket: %w", err)
	}
	srv := &standbyServer{
		s:         s,
		socket:    socket,
		listener:  listener,
		pid:       pid,
		stopWatch: make(chan struct{}),
	}
	s.standby = srv
	go srv.watch()
	go func() {
		if err := http.Serve(listener, srv); err != nil && !errors.Is(err, net.ErrClosed) {
			log.L.Warningf("Standby socket %q: %v", srv.socket, err)
		}
	}()
	log.L.Infof("Standby sandbox (PID %d) takes a restore on %q", pid, srv.socket)
	return nil
}

// close stops serving the socket, and removes it.
func (srv *standbyServer) close() {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.closed {
		return
	}
	srv.closed = true
	srv.listener.Close()
	os.Remove(srv.socket)
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
	srv.mu.Lock()
	requested := srv.requested
	srv.requested = true
	srv.mu.Unlock()
	if requested {
		http.Error(w, "a restore was requested of this standby already", http.StatusConflict)
		return
	}
	defer srv.close()
	// A client gone does not stop the restore: its containers would be left
	// half restored.
	restored, err := srv.restore(context.WithoutCancel(r.Context()), &req)
	if err != nil {
		log.L.Warningf("Restore into standby sandbox (PID %d) failed: %v", srv.pid, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(restored); err != nil {
		log.L.Warningf("Answering the restore into standby sandbox (PID %d): %v", srv.pid, err)
	}
}

// restore restores the checkpoint req names into every container of the
// standby, in the order they were created.
//
// +checklocksexclude:srv.s.mu
func (srv *standbyServer) restore(ctx context.Context, req *standbyRestoreRequest) (*standbyRestored, error) {
	// From here on, the restored containers' runsc wait reports their exits.
	close(srv.stopWatch)
	if fi, err := os.Stat(req.ImagePath); err != nil || !fi.IsDir() {
		srv.fail(nil)
		return nil, fmt.Errorf("checkpoint %q is not a directory: %v", req.ImagePath, err)
	}
	var restored standbyRestored
	for _, c := range srv.s.createdContainers() {
		conf := c.takeStandby()
		if conf == nil {
			log.L.Infof("Container %q of the standby sandbox (PID %d) is not a standby's: not restored", c.ID, srv.pid)
			continue
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
	if err := unix.Kill(srv.pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		log.L.Warningf("Killing standby sandbox (PID %d): %v", srv.pid, err)
	}
	if failed != nil {
		srv.s.ec <- proc.Exit{Timestamp: time.Now(), ID: failed.ID, Status: proc.InternalErrorCode}
	}
	for _, c := range srv.s.createdContainers() {
		if c.takeStandby() != nil {
			srv.s.ec <- proc.Exit{Timestamp: time.Now(), ID: c.ID, Status: proc.InternalErrorCode}
		}
	}
}

// watch reports the exit of every container of the standby that is one still
// once its sandbox's process exits, until stopWatch is closed.
//
// +checklocksexclude:srv.s.mu
func (srv *standbyServer) watch() {
	pidfd, err := unix.PidfdOpen(srv.pid, 0)
	if err != nil {
		log.L.Warningf("Watching standby sandbox (PID %d): %v", srv.pid, err)
		return
	}
	defer unix.Close(pidfd)
	for {
		select {
		case <-srv.stopWatch:
			return
		default:
		}
		fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 100 /* ms */)
		if err != nil && !errors.Is(err, unix.EINTR) {
			log.L.Warningf("Watching standby sandbox (PID %d): %v", srv.pid, err)
			return
		}
		if n == 0 {
			continue
		}
		state, alive := processState(srv.pid)
		if alive {
			// Seen while the sandbox execs itself early in its boot, from a
			// thread other than its leader: keep watching.
			log.L.Warningf("Standby sandbox (PID %d) polled exited (events %#x) but is alive (%s)", srv.pid, fds[0].Revents, state)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		log.L.Infof("Standby sandbox (PID %d) exited before a restore into it (events %#x, %s)", srv.pid, fds[0].Revents, state)
		srv.close()
		for _, c := range srv.s.createdContainers() {
			if c.takeStandby() != nil {
				srv.s.ec <- proc.Exit{Timestamp: time.Now(), ID: c.ID, Status: 128 + int(unix.SIGKILL)}
			}
		}
		return
	}
}

// processState returns the state of the process pid, as /proc/pid/stat says
// it, and whether it is alive: neither a zombie nor gone.
func processState(pid int) (string, bool) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return err.Error(), false
	}
	// The state follows the command, which is in parentheses.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 || i+2 >= len(stat) {
		return string(stat), false
	}
	state := stat[i+2]
	return fmt.Sprintf("state %c", state), state != 'Z' && state != 'X'
}

// killStandby kills the container c of a standby, which runs nothing: the
// sandbox, if c is its root container, whose exit watch reports; c alone
// otherwise, whose exit is reported at once.
//
// +checklocksexclude:s.mu
func (s *runscService) killStandby(c *Container, signal uint32) {
	log.L.Infof("Signal %d to container %q of a standby sandbox, which runs nothing", signal, c.ID)
	p, err := c.Process("")
	if err != nil {
		return
	}
	if initProcess, ok := p.(*proc.Init); ok && initProcess.Sandbox {
		if err := unix.Kill(initProcess.Pid(), unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
			log.L.Warningf("Killing standby sandbox (PID %d): %v", initProcess.Pid(), err)
		}
		return
	}
	if c.takeStandby() != nil {
		s.ec <- proc.Exit{Timestamp: time.Now(), ID: c.ID, Status: 128 + int(signal)}
	}
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
