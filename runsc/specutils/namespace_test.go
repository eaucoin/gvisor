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

package specutils

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// applyNSHelperEnv makes the test binary run applyNSHelper instead of the
// tests.
const applyNSHelperEnv = "SPECUTILS_TEST_APPLY_NS_HELPER"

// Exit statuses of applyNSHelper besides 0.
const (
	applyNSHelperFailed      = 1
	applyNSHelperUnsupported = 2
)

func init() {
	if os.Getenv(applyNSHelperEnv) != "" {
		// Keep the main goroutine on the main thread, the thread group
		// leader, so that applyNSHelper's other goroutine runs on another
		// thread.
		runtime.LockOSThread()
	}
}

func TestMain(m *testing.M) {
	if os.Getenv(applyNSHelperEnv) != "" {
		os.Exit(applyNSHelper())
	}
	os.Exit(m.Run())
}

// TestApplyNSRestoresThreadNamespace checks that the function returned by
// ApplyNS brings the calling thread back to its own namespace, rather than to
// the thread group leader's, when the two differ.
func TestApplyNSRestoresThreadNamespace(t *testing.T) {
	// Run the helper as root in new user and network namespaces, so that it
	// can create and join network namespaces.
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), applyNSHelperEnv+"=1")
	cmd.SysProcAttr = &unix.SysProcAttr{
		Cloneflags: unix.CLONE_NEWUSER | unix.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
	}
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr) && exitErr.ExitCode() == applyNSHelperUnsupported:
		t.Skipf("Cannot create network namespaces in a user namespace:\n%s", out)
	case errors.As(err, &exitErr):
		t.Fatalf("Helper failed: %v\n%s", err, out)
	case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES), errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSPC), errors.Is(err, unix.EUSERS):
		t.Skipf("Cannot create user and network namespaces: %v", err)
	default:
		t.Fatalf("Cannot start helper: %v", err)
	}
}

// applyNSHelper runs ApplyNS and its restore function on a thread in another
// network namespace than the main thread's, and returns the exit status of the
// helper process.
func applyNSHelper() int {
	type result struct {
		status int
		msg    string
	}
	resc := make(chan result)
	go func() {
		// Never unlocked: the thread exits with the goroutine, rather than
		// returning to the pool in other namespaces.
		runtime.LockOSThread()
		status, msg := applyNSOnThread()
		resc <- result{status, msg}
	}()
	res := <-resc
	if res.msg != "" {
		fmt.Println(res.msg)
	}
	return res.status
}

func applyNSOnThread() (int, string) {
	const threadNS = "/proc/thread-self/ns/net"
	if unix.Gettid() == unix.Getpid() {
		return applyNSHelperFailed, "the helper goroutine runs on the main thread"
	}

	// Create the namespace to apply, and keep it open.
	if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
		return applyNSHelperUnsupported, fmt.Sprintf("unshare(CLONE_NEWNET): %v", err)
	}
	target, err := os.Open(threadNS)
	if err != nil {
		return applyNSHelperFailed, err.Error()
	}
	defer target.Close()
	targetInfo, err := target.Stat()
	if err != nil {
		return applyNSHelperFailed, err.Error()
	}

	// Move this thread, and only it, to the namespace to restore.
	if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
		return applyNSHelperFailed, fmt.Sprintf("unshare(CLONE_NEWNET): %v", err)
	}
	orig, err := os.Stat(threadNS)
	if err != nil {
		return applyNSHelperFailed, err.Error()
	}
	leader, err := os.Stat("/proc/self/ns/net")
	if err != nil {
		return applyNSHelperFailed, err.Error()
	}
	if os.SameFile(orig, leader) {
		return applyNSHelperFailed, "the thread is in the main thread's network namespace"
	}

	restore, err := ApplyNS(specs.LinuxNamespace{
		Type: specs.NetworkNamespace,
		Path: fmt.Sprintf("/proc/self/fd/%d", target.Fd()),
	})
	if err != nil {
		return applyNSHelperFailed, fmt.Sprintf("ApplyNS: %v", err)
	}
	applied, err := os.Stat(threadNS)
	if err != nil {
		return applyNSHelperFailed, err.Error()
	}
	if !os.SameFile(applied, targetInfo) {
		return applyNSHelperFailed, "ApplyNS did not move the thread to the given network namespace"
	}

	if err := restore(); err != nil {
		return applyNSHelperFailed, fmt.Sprintf("restoring the network namespace: %v", err)
	}
	restored, err := os.Stat(threadNS)
	if err != nil {
		return applyNSHelperFailed, err.Error()
	}
	switch {
	case os.SameFile(restored, orig):
		return 0, ""
	case os.SameFile(restored, leader):
		return applyNSHelperFailed, "the thread was restored to the main thread's network namespace, not its own"
	default:
		return applyNSHelperFailed, "the thread was restored to another network namespace than its own"
	}
}
