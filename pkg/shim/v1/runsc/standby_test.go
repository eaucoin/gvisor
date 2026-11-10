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
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/stdio"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/shim/v1/extension"
	"gvisor.dev/gvisor/pkg/shim/v1/proc"
	"gvisor.dev/gvisor/pkg/shim/v1/runsccmd"
	"gvisor.dev/gvisor/runsc/specutils"
)

func init() {
	// Set before any test runs, and never again: the watch of a test's
	// standby may outlive it.
	closeStandbyStdio = recordStandbyStdio
}

// stdioRecords are the records, by container, of the I/O closed of the
// containers that closedStdio was asked to record it of.
var stdioRecords sync.Map // *Container -> func(*Container)

// recordStandbyStdio records the I/O of c closed where a test asked it to,
// and closes it otherwise.
func recordStandbyStdio(c *Container) {
	if record, ok := stdioRecords.Load(c); ok {
		record.(func(*Container))(c)
		return
	}
	closeStdio(c)
}

// standbyContainer returns a container of a standby, which no restore has been
// made into, and which is not its sandbox's own.
func standbyContainer(id string) *Container {
	return &Container{ID: id, task: &proc.Init{}, standby: &extension.RestoreConfig{}}
}

// standbySandboxContainer returns the root container of a standby, which no
// restore has been made into, whose sandbox's process is pid.
func standbySandboxContainer(t *testing.T, id string, pid int) *Container {
	t.Helper()
	sandbox, err := openStandbySandbox(pid)
	if err != nil {
		t.Fatalf("openStandbySandbox: %v", err)
	}
	t.Cleanup(sandbox.close)
	return &Container{ID: id, task: &proc.Init{Sandbox: true}, standby: &extension.RestoreConfig{}, standbySandbox: sandbox}
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
		ec:         make(chan proc.Exit, len(containers)+1),
	}
}

// closedStdio records, for the test, the containers of s whose I/O is closed,
// and how many exits were reported, unread, as it was; it returns the
// containers, in order, and those counts.
func closedStdio(t *testing.T, s *runscService) func() ([]string, []int) {
	t.Helper()
	var (
		mu     sync.Mutex
		closed []string
		queued []int
	)
	record := func(c *Container) {
		mu.Lock()
		defer mu.Unlock()
		closed = append(closed, c.ID)
		queued = append(queued, len(s.ec))
	}
	for _, c := range s.createdContainers() {
		stdioRecords.Store(c, record)
		t.Cleanup(func() { stdioRecords.Delete(c) })
	}
	return func() ([]string, []int) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(closed), slices.Clone(queued)
	}
}

// socketDir returns a directory for a standby's socket, whose path, unlike
// that of t.TempDir, is not made longer than a socket's may be by the test's
// name; it is removed at the end of the test.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "standby")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// serve serves the standby of s on socket for c, as Start does, until the end
// of the test.
func serve(t *testing.T, s *runscService, c *Container, socket string) *standbyServer {
	t.Helper()
	srv, err := s.serveStandby(c, socket)
	if err != nil {
		t.Fatalf("serveStandby: %v", err)
	}
	t.Cleanup(srv.close)
	srv.run()
	return srv
}

// sandboxProcess starts a process that stands for a standby's sandbox, and
// returns its PID, and a function that kills and reaps it, as the end of the
// test does.
func sandboxProcess(t *testing.T) (int, func()) {
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
	stop := func() {
		cmd.Process.Kill()
		<-waited
	}
	t.Cleanup(stop)
	return cmd.Process.Pid, stop
}

// waitDead waits for the process pid to be dead.
func waitDead(t *testing.T, sandbox *standbySandbox) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, alive := sandbox.alive(); !alive {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sandbox (PID %d) is alive", sandbox.pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
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

func TestValidStandbySocket(t *testing.T) {
	for _, tc := range []struct {
		socket string
		valid  bool
	}{
		{standbySocketDir + "/pod.sock", true},
		{standbySocketDir + "/pod-1_a.b.sock", true},
		{standbySocketDir + "/pod", false},
		{standbySocketDir + "/.sock", false},
		{standbySocketDir + "/../pod.sock", false},
		{standbySocketDir + "/dir/pod.sock", false},
		{standbySocketDir + "//pod.sock", false},
		{standbySocketDir + "/../standby/pod.sock", false},
		{filepath.Dir(standbySocketDir) + "/link/../standby/pod.sock", false},
		{"/run/pod.sock", false},
		{"/tmp/pod.sock", false},
		{"pod.sock", false},
	} {
		if err := validStandbySocket(tc.socket); (err == nil) != tc.valid {
			t.Errorf("validStandbySocket(%q) = %v, want valid %t", tc.socket, err, tc.valid)
		}
	}
}

func TestContainerStandbySocket(t *testing.T) {
	const socket = standbySocketDir + "/pod.sock"
	got, err := containerStandbySocket(socket, "app")
	if want := standbySocketDir + "/pod.app.sock"; err != nil || got != want {
		t.Errorf("containerStandbySocket(%q, app) = %q, %v, want %q", socket, got, err, want)
	}
	for _, name := range []string{"", "a/b", "..", "../app", ".app"} {
		if got, err := containerStandbySocket(socket, name); err == nil {
			t.Errorf("containerStandbySocket(%q, %q) = %q, want an error", socket, name, got)
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
	pid, _ := sandboxProcess(t)
	pause := standbySandboxContainer(t, "pause", pid)
	s := standbyService(pause)
	socket := filepath.Join(socketDir(t), "standby", "pod.sock")
	serve(t, s, pause, socket)

	fi, err := os.Stat(socket)
	if err != nil {
		t.Fatalf("the standby's socket: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("the standby's socket's mode is %o, want 600", perm)
	}
	if srv, err := s.serveStandby(pause, filepath.Join(socketDir(t), "other.sock")); err == nil {
		srv.close()
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

func TestStandbySocketTakesOnlyASocketsPlace(t *testing.T) {
	pid, _ := sandboxProcess(t)
	for _, tc := range []struct {
		name string
		// place puts something at socket, in dir, before it is served.
		place func(t *testing.T, dir, socket string)
		ok    bool
	}{
		{
			name:  "nothing",
			place: func(*testing.T, string, string) {},
			ok:    true,
		},
		{
			name: "a socket no one listens on",
			place: func(t *testing.T, _, socket string) {
				l, err := net.Listen("unix", socket)
				if err != nil {
					t.Fatal(err)
				}
				l.(*net.UnixListener).SetUnlinkOnClose(false)
				l.Close()
			},
			ok: true,
		},
		{
			name: "a socket listened on",
			place: func(t *testing.T, _, socket string) {
				l, err := net.Listen("unix", socket)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { l.Close() })
			},
		},
		{
			name: "a file",
			place: func(t *testing.T, _, socket string) {
				if err := os.WriteFile(socket, []byte("kept"), 0600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a symbolic link",
			place: func(t *testing.T, dir, socket string) {
				if err := os.Symlink(filepath.Join(dir, "elsewhere"), socket); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a directory",
			place: func(t *testing.T, _, socket string) {
				if err := os.Mkdir(socket, 0700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "in a directory others may write to",
			place: func(t *testing.T, dir, _ string) {
				if err := os.Chmod(dir, 0777); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(socketDir(t), "standby")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			socket := filepath.Join(dir, "pod.sock")
			tc.place(t, dir, socket)
			before, _ := os.Lstat(socket)
			pause := standbySandboxContainer(t, "pause", pid)
			s := standbyService(pause)
			srv, err := s.serveStandby(pause, socket)
			if tc.ok {
				if err != nil {
					t.Fatalf("serveStandby: %v", err)
				}
				srv.close()
				return
			}
			if err == nil {
				srv.close()
				t.Fatalf("serveStandby succeeded")
			}
			if standbyServed(s) != nil {
				t.Errorf("the shim holds the standby's server after it failed")
			}
			if after, _ := os.Lstat(socket); (before == nil) != (after == nil) || (before != nil && !os.SameFile(before, after)) {
				t.Errorf("what was at the socket's path was replaced")
			}
		})
	}
}

// standbyServed returns the standby's server of s.
func standbyServed(s *runscService) *standbyServer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.standby
}

func TestStandbyFailedRestoreStopsSandbox(t *testing.T) {
	pid, _ := sandboxProcess(t)
	pause := standbySandboxContainer(t, "pause", pid)
	s := standbyService(pause, standbyContainer("app"))
	closed := closedStdio(t, s)
	socket := filepath.Join(socketDir(t), "pod.sock")
	serve(t, s, pause, socket)

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
	if got, _ := closed(); !slices.Equal(got, []string{"pause", "app"}) {
		t.Errorf("the I/O closed of %v, want of pause and app", got)
	}
	waitDead(t, pause.standbySandbox)
	// The socket took its one request, and is gone.
	waitRemoved(t, socket)
}

func TestStandbyRestoreFailsOfAContainerKilled(t *testing.T) {
	pid, _ := sandboxProcess(t)
	pause := standbySandboxContainer(t, "pause", pid)
	app := standbyContainer("app")
	// The restore restores app first, so that it finds app killed before it
	// calls runsc for pause.
	s := standbyService(app, pause)
	socket := filepath.Join(socketDir(t), "pod.sock")
	serve(t, s, pause, socket)
	// A Kill of app as the restore is requested.
	if !s.killStandby(app, uint32(unix.SIGTERM)) {
		t.Fatalf("killStandby of a standby's container: false")
	}
	if got := exits(s, 1); len(got) != 1 || got["app"] != 128+int(unix.SIGTERM) {
		t.Fatalf("exits reported of the kill: %v, want app's", got)
	}
	status, body := postRestore(t, socket, http.MethodPost, "/restore", `{"imagePath": "`+t.TempDir()+`"}`)
	if status != http.StatusInternalServerError || !strings.Contains(body, `"app"`) {
		t.Fatalf("restore with a container killed: status %d (%q), want %d naming app", status, body, http.StatusInternalServerError)
	}
	if got := exits(s, 1); len(got) != 1 || got["pause"] != proc.InternalErrorCode {
		t.Errorf("exits reported: %v, want pause's at %d", got, proc.InternalErrorCode)
	}
	waitDead(t, pause.standbySandbox)
	waitRemoved(t, socket)
}

func TestStandbyRefusesSecondRequest(t *testing.T) {
	pid, _ := sandboxProcess(t)
	pause := standbySandboxContainer(t, "pause", pid)
	s := standbyService(pause)
	socket := filepath.Join(socketDir(t), "pod.sock")
	srv := serve(t, s, pause, socket)
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
	pid, stop := sandboxProcess(t)
	pause := standbySandboxContainer(t, "pause", pid)
	restored := standbyContainer("restored")
	restored.takeStandby()
	s := standbyService(pause, restored, standbyContainer("app"))
	closed := closedStdio(t, s)
	socket := filepath.Join(socketDir(t), "pod.sock")
	serve(t, s, pause, socket)
	if got := exits(s, 0); len(got) != 0 {
		t.Fatalf("exits reported while the sandbox runs: %v", got)
	}
	stop()
	got := exits(s, 2)
	want := map[string]int{"pause": 128 + int(unix.SIGKILL), "app": 128 + int(unix.SIGKILL)}
	if len(got) != len(want) || got["pause"] != want["pause"] || got["app"] != want["app"] {
		t.Errorf("exits reported: %v, want %v (none of a container restored)", got, want)
	}
	if got, _ := closed(); !slices.Equal(got, []string{"pause", "app"}) {
		t.Errorf("the I/O closed of %v, want of pause and app (not of the container restored)", got)
	}
	waitRemoved(t, socket)
}

func TestStandbyExitReportedOfSandboxGoneBeforeStart(t *testing.T) {
	pid, stop := sandboxProcess(t)
	// Created while the sandbox ran.
	pause := standbySandboxContainer(t, "pause", pid)
	s := standbyService(pause, standbyContainer("app"))
	// Gone, and reaped, before its container started.
	stop()
	socket := filepath.Join(socketDir(t), "pod.sock")
	serve(t, s, pause, socket)
	got := exits(s, 2)
	if len(got) != 2 || got["pause"] != 128+int(unix.SIGKILL) || got["app"] != 128+int(unix.SIGKILL) {
		t.Errorf("exits reported: %v, want pause's and app's at %d", got, 128+int(unix.SIGKILL))
	}
	waitRemoved(t, socket)
}

func TestStandbyServedNoExitBeforeRun(t *testing.T) {
	pid, stop := sandboxProcess(t)
	pause := standbySandboxContainer(t, "pause", pid)
	s := standbyService(pause)
	stop()
	srv, err := s.serveStandby(pause, filepath.Join(socketDir(t), "pod.sock"))
	if err != nil {
		t.Fatalf("serveStandby: %v", err)
	}
	t.Cleanup(srv.close)
	// Start reports the container started before it runs the server.
	if got := exits(s, 0); len(got) != 0 {
		t.Fatalf("exits reported before the server ran: %v", got)
	}
	srv.run()
	if got := exits(s, 1); got["pause"] != 128+int(unix.SIGKILL) {
		t.Errorf("exits reported once the server ran: %v, want pause's", got)
	}
}

func TestKillStandbyContainer(t *testing.T) {
	c := standbyContainer("app")
	s := standbyService(c)
	closed := closedStdio(t, s)
	// Not the sandbox's own container: its exit is reported at once, its I/O,
	// which runsc never took, closed first, as CRI waits for its end to stop it.
	if !s.killStandby(c, uint32(unix.SIGTERM)) {
		t.Fatalf("killStandby of a standby's container: false")
	}
	got := exits(s, 1)
	if status, ok := got["app"]; !ok || status != 128+int(unix.SIGTERM) {
		t.Errorf("exits reported: %v, want app's at %d", got, 128+int(unix.SIGTERM))
	}
	if c.isStandby() {
		t.Errorf("a killed container is a standby's still")
	}
	// Closed before its exit was reported.
	if got, queued := closed(); !slices.Equal(got, []string{"app"}) || !slices.Equal(queued, []int{0}) {
		t.Errorf("the I/O closed of %v, with %v exits reported already; want of app, with none", got, queued)
	}
	// Killed again, as kubelet may, or taken by a restore: Kill kills it as it
	// does any container.
	if s.killStandby(c, uint32(unix.SIGKILL)) {
		t.Errorf("killStandby of a container a standby's no more: true")
	}
	if got := exits(s, 0); len(got) != 0 {
		t.Errorf("exits reported of a second kill: %v", got)
	}
	if got, _ := closed(); len(got) != 1 {
		t.Errorf("the I/O closed of %v, once more", got)
	}
}

func TestKillStandbySandbox(t *testing.T) {
	pid, _ := sandboxProcess(t)
	pause := standbySandboxContainer(t, "pause", pid)
	s := standbyService(pause)
	if !s.killStandby(pause, uint32(unix.SIGTERM)) {
		t.Fatalf("killStandby of a standby's root container: false")
	}
	waitDead(t, pause.standbySandbox)
	// Its exit is watch's to report.
	if got := exits(s, 0); len(got) != 0 {
		t.Errorf("exits reported of a kill of the sandbox: %v", got)
	}
}

func TestStandbySandboxReaped(t *testing.T) {
	pid, stop := sandboxProcess(t)
	sandbox, err := openStandbySandbox(pid)
	if err != nil {
		t.Fatalf("openStandbySandbox: %v", err)
	}
	defer sandbox.close()
	if state, alive := sandbox.alive(); !alive {
		t.Fatalf("the sandbox is not alive: %s", state)
	}
	stop()
	if state, alive := sandbox.alive(); alive {
		t.Errorf("the sandbox reaped is alive: %s", state)
	}
	// The pidfd reaches no process given its PID since.
	if err := sandbox.kill(); err != nil {
		t.Errorf("kill of the sandbox reaped: %v", err)
	}
	if _, err := openStandbySandbox(pid); err == nil {
		t.Errorf("openStandbySandbox of a process reaped succeeded")
	}
	sandbox.close()
	if err := sandbox.kill(); err == nil {
		t.Errorf("kill through a pidfd closed succeeded")
	}
	if fd, err := sandbox.dup(); err == nil {
		unix.Close(fd)
		t.Errorf("dup of a pidfd closed succeeded")
	}
}

func TestStandbySandboxAnotherProcess(t *testing.T) {
	sandbox, err := openStandbySandbox(os.Getpid())
	if err != nil {
		t.Fatalf("openStandbySandbox: %v", err)
	}
	defer sandbox.close()
	if state, alive := sandbox.alive(); !alive {
		t.Fatalf("the test's own process is not alive: %s", state)
	}
	// As where the sandbox was reaped and its PID went to another process.
	sandbox.start++
	if state, alive := sandbox.alive(); alive {
		t.Errorf("a process that started at another time is the sandbox: %s", state)
	}
}

func TestStandbyOverOnceRestoreRequested(t *testing.T) {
	pid, _ := sandboxProcess(t)
	pause := standbySandboxContainer(t, "pause", pid)
	app := standbyContainer("app")
	later := standbyContainer("later")
	s := standbyService(pause, app, later)
	srv := serve(t, s, pause, filepath.Join(socketDir(t), "pod.sock"))
	ctx := context.Background()
	for _, c := range []*Container{pause, app, later} {
		if s.standbyOver(ctx, c) {
			t.Errorf("standbyOver(%q) before a restore was requested: true", c.ID)
		}
	}
	// A restore requested of pause and app, before later was created.
	srv.mu.Lock()
	srv.requested = true
	srv.restoring = []*Container{pause, app}
	srv.mu.Unlock()
	if s.standbyOver(ctx, app) {
		t.Errorf("standbyOver of a container the restore restores: true")
	}
	if !s.standbyOver(ctx, later) {
		t.Errorf("standbyOver of a container created after the restore was requested: false")
	}
	if s.standbyOver(ctx, pause) {
		t.Errorf("standbyOver of the sandbox's own container: true")
	}
}

func TestStandbyOverOnceSandboxGone(t *testing.T) {
	pid, _ := sandboxProcess(t)
	pause := standbySandboxContainer(t, "pause", pid)
	app := standbyContainer("app")
	s := standbyService(pause, app)
	srv := serve(t, s, pause, filepath.Join(socketDir(t), "pod.sock"))
	srv.close()
	if !s.standbyOver(context.Background(), app) {
		t.Errorf("standbyOver of a container of a standby closed: false")
	}
}

func TestStandbyOverOfAShimOfItsOwn(t *testing.T) {
	for _, tc := range []struct {
		status string
		over   bool
	}{
		{"created", false},
		{"running", true},
		{"paused", true},
	} {
		t.Run(tc.status, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "fake-runsc")
			argsLog := filepath.Join(dir, "args.txt")
			body := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >>%q\necho '{\"id\": \"pod\", \"status\": %q}'\n", argsLog, tc.status)
			if err := os.WriteFile(script, []byte(body), 0755); err != nil {
				t.Fatal(err)
			}
			app := &Container{
				ID:      "app",
				Bundle:  writeBundle(t, map[string]string{specutils.ContainerdSandboxIDAnnotation: "pod"}),
				task:    proc.New("app", &runsccmd.Runsc{Command: script, Root: dir}, stdio.Stdio{}),
				standby: &extension.RestoreConfig{},
			}
			s := standbyService(app)
			s.mu.Lock()
			s.standbyNames = map[string]string{"app": "app"}
			s.mu.Unlock()
			if got := s.standbyOver(context.Background(), app); got != tc.over {
				t.Errorf("standbyOver with the root container %s: %t, want %t", tc.status, got, tc.over)
			}
			args, err := os.ReadFile(argsLog)
			if err != nil {
				t.Fatalf("runsc never ran: %v", err)
			}
			if !strings.Contains(string(args), "state pod") {
				t.Errorf("runsc ran with %q, want the state of the root container", args)
			}
		})
	}
}

func TestProcessStat(t *testing.T) {
	state, start, err := processStat(os.Getpid())
	if err != nil || state == 'Z' || state == 'X' || start == 0 {
		t.Errorf("processStat of the test itself: %c, %d, %v", state, start, err)
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("true: %v", err)
	}
	if _, _, err := processStat(cmd.Process.Pid); err == nil {
		t.Errorf("processStat of a process reaped succeeded")
	}
}
