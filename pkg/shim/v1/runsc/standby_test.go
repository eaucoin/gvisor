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
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/shim/v1/extension"
	"gvisor.dev/gvisor/pkg/shim/v1/proc"
)

// standbyContainer returns a container of a standby, which no restore has been
// made into.
func standbyContainer(id string) *Container {
	return &Container{ID: id, standby: &extension.RestoreConfig{}}
}

// standbyService returns a shim service holding the containers, created in
// the order given, and whose exits are reported on a buffered channel.
func standbyService(containers ...*Container) *runscService {
	byID := make(map[string]*Container)
	var created []string
	for _, c := range containers {
		byID[c.ID] = c
		created = append(created, c.ID)
	}
	return &runscService{
		containers: byID,
		created:    created,
		ec:         make(chan proc.Exit, len(containers)),
	}
}

// standbyServed returns the standby's server of s.
func standbyServed(s *runscService) *standbyServer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.standby
}

// sandboxProcess starts a process that stands for a standby's sandbox, and
// returns its PID; it is killed and reaped at the end of the test.
func sandboxProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the sandbox's stand-in: %v", err)
	}
	waited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(waited)
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-waited
	})
	return cmd.Process.Pid
}

// postRestore posts body to the standby's socket, and returns the answer's
// status and body.
func postRestore(t *testing.T, socket, method, path, body string) (int, string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequest(method, "http://standby"+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var answer strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		answer.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, answer.String()
}

// exits returns the exits reported on s.ec within a short wait, by container.
func exits(s *runscService, want int) map[string]int {
	got := make(map[string]int)
	deadline := time.After(5 * time.Second)
	for len(got) < want {
		select {
		case e := <-s.ec:
			got[e.ID] = e.Status
		case <-deadline:
			return got
		}
	}
	// Anything more is reported at once, as the expected exits were.
	for {
		select {
		case e := <-s.ec:
			got[e.ID] = e.Status
		case <-time.After(100 * time.Millisecond):
			return got
		}
	}
}

func TestContainerStandbySocket(t *testing.T) {
	for _, tc := range []struct {
		socket, name, want string
	}{
		{"/run/standby/pod.sock", "app", "/run/standby/pod.app.sock"},
		{"/run/standby/pod", "app", "/run/standby/pod.app.sock"},
	} {
		if got := containerStandbySocket(tc.socket, tc.name); got != tc.want {
			t.Errorf("containerStandbySocket(%q, %q) = %q, want %q", tc.socket, tc.name, got, tc.want)
		}
	}
}

func TestTakeStandbyOnce(t *testing.T) {
	c := standbyContainer("c")
	if !c.isStandby() {
		t.Fatalf("a standby's container is not one")
	}
	if c.takeStandby() == nil {
		t.Fatalf("takeStandby of a standby's container: nil")
	}
	if c.isStandby() {
		t.Errorf("a container taken is a standby's still")
	}
	if c.takeStandby() != nil {
		t.Errorf("a second takeStandby returned the restore options again")
	}
}

func TestStandbySocketServesOnlyItsRequest(t *testing.T) {
	pause := standbyContainer("pause")
	s := standbyService(pause)
	socket := filepath.Join(t.TempDir(), "standby", "pod.sock")
	if err := s.serveStandby(sandboxProcess(t), socket); err != nil {
		t.Fatalf("serveStandby: %v", err)
	}
	t.Cleanup(standbyServed(s).close)

	fi, err := os.Stat(socket)
	if err != nil {
		t.Fatalf("the standby's socket: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("the standby's socket's mode is %o, want 600", perm)
	}
	if err := s.serveStandby(sandboxProcess(t), socket); err == nil {
		t.Errorf("a second serveStandby of the shim succeeded")
	}

	for _, tc := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"get", http.MethodGet, "/restore", "", http.StatusNotFound},
		{"another path", http.MethodPost, "/resume", `{"imagePath": "/ckpt"}`, http.StatusNotFound},
		{"not json", http.MethodPost, "/restore", "{", http.StatusBadRequest},
		{"no image path", http.MethodPost, "/restore", `{"paused": true}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if status, body := postRestore(t, socket, tc.method, tc.path, tc.body); status != tc.want {
				t.Errorf("status %d (%q), want %d", status, body, tc.want)
			}
		})
	}
	// None of those was a restore: the standby takes one still.
	if !pause.isStandby() {
		t.Errorf("a refused request made the container a standby's no more")
	}
}

func TestStandbyFailedRestoreStopsSandbox(t *testing.T) {
	s := standbyService(standbyContainer("pause"), standbyContainer("app"))
	pid := sandboxProcess(t)
	socket := filepath.Join(t.TempDir(), "pod.sock")
	if err := s.serveStandby(pid, socket); err != nil {
		t.Fatalf("serveStandby: %v", err)
	}

	missing := filepath.Join(t.TempDir(), "missing")
	if status, body := postRestore(t, socket, http.MethodPost, "/restore", `{"imagePath": "`+missing+`"}`); status != http.StatusInternalServerError {
		t.Fatalf("restore from a missing checkpoint: status %d (%q), want %d", status, body, http.StatusInternalServerError)
	}
	got := exits(s, 2)
	for _, id := range []string{"pause", "app"} {
		if status, ok := got[id]; !ok || status != proc.InternalErrorCode {
			t.Errorf("container %q: exit %d reported %t, want %d", id, status, ok, proc.InternalErrorCode)
		}
	}
	if len(got) != 2 {
		t.Errorf("exits reported: %v, want one per container", got)
	}
	// The sandbox's stand-in was killed.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, alive := processState(pid); !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sandbox (PID %d) is alive after its restore failed", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The socket took its one request, and is gone.
	waitRemoved(t, socket)
}

// waitRemoved waits for the standby's socket to be removed.
func waitRemoved(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := os.Stat(socket)
		if os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the standby's socket %q: %v, want it removed", socket, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStandbyRefusesSecondRequest(t *testing.T) {
	pause := standbyContainer("pause")
	s := standbyService(pause)
	socket := filepath.Join(t.TempDir(), "pod.sock")
	if err := s.serveStandby(sandboxProcess(t), socket); err != nil {
		t.Fatalf("serveStandby: %v", err)
	}
	srv := standbyServed(s)
	t.Cleanup(srv.close)
	// A request in flight: its handler has marked the standby requested, and
	// holds the socket open until it answers. The second request is made while
	// it is, as a client racing another would.
	srv.mu.Lock()
	srv.requested = true
	srv.mu.Unlock()
	if status, body := postRestore(t, socket, http.MethodPost, "/restore", `{"imagePath": "/ckpt"}`); status != http.StatusConflict {
		t.Errorf("a second restore request: status %d (%q), want %d", status, body, http.StatusConflict)
	}
	if !pause.isStandby() {
		t.Errorf("a refused request made the container a standby's no more")
	}
}

func TestStandbyExitReportedOnceSandboxGone(t *testing.T) {
	restored := standbyContainer("restored")
	restored.takeStandby()
	s := standbyService(standbyContainer("pause"), restored, standbyContainer("app"))
	pid := sandboxProcess(t)
	socket := filepath.Join(t.TempDir(), "pod.sock")
	if err := s.serveStandby(pid, socket); err != nil {
		t.Fatalf("serveStandby: %v", err)
	}
	if got := exits(s, 0); len(got) != 0 {
		t.Fatalf("exits reported while the sandbox runs: %v", got)
	}
	if err := unix.Kill(pid, unix.SIGKILL); err != nil {
		t.Fatalf("killing the sandbox: %v", err)
	}
	got := exits(s, 2)
	want := map[string]int{"pause": 128 + int(unix.SIGKILL), "app": 128 + int(unix.SIGKILL)}
	if len(got) != len(want) || got["pause"] != want["pause"] || got["app"] != want["app"] {
		t.Errorf("exits reported: %v, want %v (none of a container restored)", got, want)
	}
	waitRemoved(t, socket)
}

func TestKillStandbyContainer(t *testing.T) {
	c := standbyContainer("app")
	c.task = &proc.Init{}
	s := standbyService(c)
	// Not the sandbox's own container: its exit is reported at once.
	s.killStandby(c, uint32(unix.SIGTERM))
	got := exits(s, 1)
	if status, ok := got["app"]; !ok || status != 128+int(unix.SIGTERM) {
		t.Errorf("exits reported: %v, want app's at %d", got, 128+int(unix.SIGTERM))
	}
	if c.isStandby() {
		t.Errorf("a killed container is a standby's still")
	}
}

func TestProcessState(t *testing.T) {
	if state, alive := processState(os.Getpid()); !alive {
		t.Errorf("processState of the test itself: %s, not alive", state)
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("true: %v", err)
	}
	if state, alive := processState(cmd.Process.Pid); alive {
		t.Errorf("processState of a process reaped: %s, alive", state)
	}
}
