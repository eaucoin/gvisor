// Copyright 2024 The gVisor Authors.
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

package boot

import (
	"errors"
	"fmt"
	"io"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/fd"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/control"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/strace"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/runsc/config"
)

func getTargetForSaveResume(l *Loader) func(k *kernel.Kernel) {
	return func(k *kernel.Kernel) {
		// Discard the state file contents for save-resume. There is no need to
		// verify the state file, we just need the sandbox to continue running
		// after save.
		saveOpts := state.SaveOpts{
			Autosave:    true,
			Resume:      true,
			Destination: io.Discard,
		}
		if l.root.conf.CoW == config.CoWOn {
			// Copy-on-write saves write a pages file, which they write
			// while the test runs.
			saveOpts.PagesMetadata = discardWriteCloser{}
			saveOpts.PagesFile = stateio.NewIOWriter(io.Discard, 1<<20, 64, 4)
		}
		defer saveOpts.Close()
		if err := l.saveWithOpts(&saveOpts, &control.SaveRestoreExecOpts{}); errors.Is(err, kernel.ErrDirtyTrackingEscapes) || errors.Is(err, pgalloc.ErrCoWEscapes) {
			// The save is complete, and the test would otherwise go on from
			// it as if nothing had escaped.
			k.Kill(linux.WaitStatusExit(1))
		}
	}
}

// discardWriteCloser is an io.WriteCloser that discards what is written to it.
type discardWriteCloser struct{}

// Write implements io.Writer.Write.
func (discardWriteCloser) Write(p []byte) (int, error) {
	return len(p), nil
}

// Close implements io.Closer.Close.
func (discardWriteCloser) Close() error {
	return nil
}

func getTargetForSaveRestore(l *Loader, files []*fd.FD) func(k *kernel.Kernel) {
	if len(files) != 1 && len(files) != 3 {
		panic(fmt.Sprintf("Unexpected number of files: %v", len(files)))
	}
	if l.root.conf.TestOnlyAutosaveKind == config.AutosaveIncremental && len(files) != 3 {
		panic("Incremental auto saves require a pages file (--TESTONLY-autosave-compression=none)")
	}

	var once sync.Once
	return func(k *kernel.Kernel) {
		once.Do(func() {
			saveOpts := state.SaveOpts{
				Autosave:    true,
				Resume:      false,
				Destination: files[0],
			}
			if len(files) == 3 {
				saveOpts.PagesMetadata = stateio.NewBufioWriteCloser(files[1])
				saveOpts.PagesFile = stateio.NewPagesFileFDWriterDefault(int32(files[2].Release()))
				if l.root.conf.TestOnlyAutosaveKind == config.AutosaveIncremental {
					if parent, ok := k.LastImageDigest(); ok {
						saveOpts.Parent = &parent
					}
				}
			}
			defer saveOpts.Close()
			l.saveWithOpts(&saveOpts, &control.SaveRestoreExecOpts{})
		})
	}
}

// enableAutosave enables auto save restore in syscall tests.
func enableAutosave(l *Loader, isResume bool, files []*fd.FD) error {
	var target func(k *kernel.Kernel)
	if isResume {
		target = getTargetForSaveResume(l)
	} else {
		target = getTargetForSaveRestore(l, files)
	}

	for _, table := range kernel.SyscallTables() {
		sys, ok := strace.Lookup(table.OS, table.Arch)
		if !ok {
			continue
		}
		if err := configureInitSyscall(table, sys, "init_module", kernel.ExternalAfterEnable); err != nil {
			return err
		}
		// Set external args to our closure above.
		table.External = target
	}

	return nil
}

// configureInitSyscall sets the trigger for the S/R syscall tests and the callback
// method to be called after the sycall is executed.
func configureInitSyscall(table *kernel.SyscallTable, sys strace.SyscallMap, initSyscall string, syscallFlag uint32) error {
	sl := make(map[uintptr]bool)
	sysno, ok := sys.ConvertToSysno(initSyscall)
	if !ok {
		return fmt.Errorf("syscall %q not found", initSyscall)
	}
	sl[sysno] = true
	log.Infof("sysno %v name %v", sysno, initSyscall)
	table.FeatureEnable.Enable(syscallFlag, sl, false)
	table.ExternalFilterBefore = func(*kernel.Task, uintptr, arch.SyscallArguments) bool {
		return false
	}
	// Sets ExternalFilterAfter to true which calls the closure assigned to
	// External after the syscall is executed.
	table.ExternalFilterAfter = func(*kernel.Task, uintptr, arch.SyscallArguments) bool {
		return true
	}
	return nil
}
