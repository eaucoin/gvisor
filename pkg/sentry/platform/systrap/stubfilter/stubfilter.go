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

// Package stubfilter defines the seccomp-bpf programs that the systrap
// platform installs in its stub processes and threads.
package stubfilter

import (
	"fmt"
	"maps"
	"slices"

	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/seccomp"
	"gvisor.dev/gvisor/pkg/seccomp/precompiledseccomp"
	"gvisor.dev/gvisor/pkg/sentry/hostmm"
)

// Program names.
const (
	// StubProcessName is the program installed in stub processes.
	StubProcessName = "systrap_stub_process"

	// StubProcessWriteTrackingName is the program installed in stub processes
	// when the platform tracks writes (see systrap's write_tracking.go).
	StubProcessWriteTrackingName = "systrap_stub_process_write_tracking"

	// SysmsgThreadName is the program installed in sysmsg threads.
	SysmsgThreadName = "systrap_sysmsg_thread"

	// SyscallThreadName is the program installed in syscall threads.
	SyscallThreadName = "systrap_syscall_thread"
)

// 64-bit variable names.
const (
	stubStartVar       = "stub_start"
	stubROMapLenVar    = "stub_ro_map_len"
	stubSysmsgStartVar = "stub_sysmsg_start"
)

// StubProcessVars returns the variables of the StubProcessName program.
func StubProcessVars(stubStart, stubROMapLen uint64) precompiledseccomp.Values {
	vars := precompiledseccomp.Values{}
	vars.SetUint64(stubStartVar, stubStart)
	vars.SetUint64(stubROMapLenVar, stubROMapLen)
	return vars
}

// SysmsgThreadVars returns the variables of the SysmsgThreadName program.
func SysmsgThreadVars(stubStart, stubSysmsgStart uint64) precompiledseccomp.Values {
	vars := precompiledseccomp.Values{}
	vars.SetUint64(stubSysmsgStartVar, stubSysmsgStart)
	setSysmsgThreadArchVars(vars, stubStart)
	return vars
}

// StubProcess returns the program that allows only the system calls needed by
// the stub and all its children.
func StubProcess(vars precompiledseccomp.Values) *seccomp.Program {
	stubStart := vars.GetUint64(stubStartVar)
	rules := []seccomp.RuleSet{{
		Rules: seccomp.MakeSyscallRules(map[uintptr]seccomp.SyscallRule{
			unix.SYS_CLONE: seccomp.Or{
				// Allow creation of new subprocesses (used by the master).
				seccomp.PerArg{seccomp.EqualTo(unix.CLONE_FILES | unix.CLONE_PARENT | unix.SIGCHLD)},
				seccomp.PerArg{seccomp.EqualTo(unix.CLONE_FILES | unix.SIGCHLD)},
				// Allow creation of new sysmsg thread.
				seccomp.PerArg{seccomp.EqualTo(
					unix.CLONE_FILES |
						unix.CLONE_FS |
						unix.CLONE_VM |
						unix.CLONE_PTRACE |
						linux.SIGKILL)},
			},

			// For the initial process creation.
			unix.SYS_WAIT4: seccomp.MatchAll{},
			unix.SYS_EXIT:  seccomp.MatchAll{},

			// For the stub prctl dance (all).
			unix.SYS_PRCTL: seccomp.Or{
				seccomp.PerArg{seccomp.EqualTo(unix.PR_SET_PDEATHSIG), seccomp.EqualTo(unix.SIGKILL)},
				seccomp.PerArg{seccomp.EqualTo(linux.PR_SET_NO_NEW_PRIVS), seccomp.EqualTo(1)},
				seccomp.PerArg{
					seccomp.EqualTo(unix.PR_SET_SYSCALL_USER_DISPATCH),
					seccomp.EqualTo(unix.PR_SYS_DISPATCH_ON),
					seccomp.EqualTo(stubStart),
					seccomp.EqualTo(vars.GetUint64(stubROMapLenVar)),
				},
			},
			unix.SYS_GETPPID: seccomp.MatchAll{},

			// For the stub to stop itself (all).
			unix.SYS_GETPID: seccomp.MatchAll{},
			unix.SYS_KILL: seccomp.PerArg{
				seccomp.AnyValue{},
				seccomp.EqualTo(unix.SIGSTOP),
			},

			// Injected to support the address space operations.
			unix.SYS_MMAP:   seccomp.MatchAll{},
			unix.SYS_MUNMAP: seccomp.MatchAll{},

			// For sysmsg threads. Look at sysmsg/sighandler.c for more details.
			unix.SYS_RT_SIGRETURN: seccomp.MatchAll{},
			unix.SYS_SCHED_YIELD:  seccomp.MatchAll{},
			unix.SYS_FUTEX: seccomp.Or{
				seccomp.PerArg{
					seccomp.AnyValue{},
					seccomp.EqualTo(linux.FUTEX_WAIT),
					seccomp.AnyValue{},
					seccomp.AnyValue{},
				},
				seccomp.PerArg{
					seccomp.AnyValue{},
					seccomp.EqualTo(linux.FUTEX_WAKE),
					seccomp.AnyValue{},
					seccomp.AnyValue{},
				},
			},
			unix.SYS_SIGALTSTACK: seccomp.MatchAll{},
			unix.SYS_TKILL: seccomp.PerArg{
				seccomp.AnyValue{},
				seccomp.EqualTo(unix.SIGSTOP),
			},
			unix.SYS_GETTID:     seccomp.MatchAll{},
			unix.SYS_EXIT_GROUP: seccomp.MatchAll{},
			seccomp.SYS_SECCOMP: seccomp.Or{
				seccomp.PerArg{
					seccomp.EqualTo(linux.SECCOMP_SET_MODE_FILTER),
					seccomp.EqualTo(0),
					seccomp.AnyValue{},
				},
				seccomp.PerArg{
					seccomp.EqualTo(linux.SECCOMP_SET_MODE_FILTER),
					seccomp.EqualTo(linux.SECCOMP_FILTER_FLAG_NEW_LISTENER),
					seccomp.AnyValue{},
				},
			},
		}),
		Action: seccomp.Allow,
	}}
	return &seccomp.Program{
		RuleSets: slices.Concat(rules, stubProcessArchRules()),
		Options: seccomp.ProgramOptions{
			DefaultAction: seccomp.Trap,
			BadArchAction: seccomp.Trap,
		},
	}
}

// StubProcessWriteTracking returns StubProcess's program, with the system
// calls that the Sentry injects in stubs to track writes; application code
// cannot make them, since the filter of the stub threads that run it traps
// every system call.
//
//   - userfaultfd, with exactly the flags of hostmm.UserfaultfdFlags: a stub
//     creates the userfaultfd of its address space, which the Sentry uses
//     through the FD table that stubs share with it.
//   - mprotect, to the protections that write tracking gives tracked
//     mappings: a mapping is mapped without write access until it is
//     write-protected, then given its protection, which includes PROT_WRITE;
//     and it is made inaccessible (PROT_NONE) before the harvest that precedes
//     its removal, so that no write lands in between. Other protections, such
//     as making a mapping executable without write access, are refused.
func StubProcessWriteTracking(vars precompiledseccomp.Values) *seccomp.Program {
	program := StubProcess(vars)
	mprotect := seccomp.Or{
		seccomp.PerArg{seccomp.AnyValue{}, seccomp.AnyValue{}, seccomp.EqualTo(unix.PROT_NONE)},
	}
	for _, at := range []hostarch.AccessType{hostarch.Write, hostarch.ReadWrite, {Write: true, Execute: true}, hostarch.AnyAccess} {
		mprotect = append(mprotect, seccomp.PerArg{seccomp.AnyValue{}, seccomp.AnyValue{}, seccomp.EqualTo(at.Prot())})
	}
	program.RuleSets[0].Rules.Merge(seccomp.MakeSyscallRules(map[uintptr]seccomp.SyscallRule{
		unix.SYS_USERFAULTFD: seccomp.PerArg{seccomp.EqualTo(hostmm.UserfaultfdFlags)},
		unix.SYS_MPROTECT:    mprotect,
	}))
	return program
}

// SysmsgThread returns the program that only allows system calls made from
// the sysmsg code stub.
func SysmsgThread(vars precompiledseccomp.Values) *seccomp.Program {
	stubSysmsgStart := vars.GetUint64(stubSysmsgStartVar)
	rules := []seccomp.RuleSet{{
		// Allow instructions from the sysmsg code stub, which is limited by one page.
		Rules: seccomp.MakeSyscallRules(map[uintptr]seccomp.SyscallRule{
			unix.SYS_FUTEX: seccomp.Or{
				seccomp.PerArg{
					seccomp.GreaterThan(stubSysmsgStart),
					seccomp.EqualTo(linux.FUTEX_WAKE),
					seccomp.EqualTo(1),
					seccomp.EqualTo(0),
					seccomp.EqualTo(0),
					seccomp.EqualTo(0),
					seccomp.GreaterThan(stubSysmsgStart), // rip
				},
				seccomp.PerArg{
					seccomp.GreaterThan(stubSysmsgStart),
					seccomp.EqualTo(linux.FUTEX_WAIT),
					seccomp.AnyValue{},
					seccomp.EqualTo(0),
					seccomp.EqualTo(0),
					seccomp.EqualTo(0),
					seccomp.GreaterThan(stubSysmsgStart), // rip
				},
			},
			unix.SYS_RT_SIGRETURN: seccomp.PerArg{
				seccomp.AnyValue{},
				seccomp.AnyValue{},
				seccomp.AnyValue{},
				seccomp.AnyValue{},
				seccomp.AnyValue{},
				seccomp.AnyValue{},
				seccomp.GreaterThan(stubSysmsgStart), // rip
			},
			unix.SYS_SCHED_YIELD: seccomp.PerArg{
				seccomp.AnyValue{},
				seccomp.AnyValue{},
				seccomp.AnyValue{},
				seccomp.AnyValue{},
				seccomp.AnyValue{},
				seccomp.AnyValue{},
				seccomp.GreaterThan(stubSysmsgStart), // rip
			},
		}),
		Action: seccomp.Allow,
	}}
	return &seccomp.Program{
		RuleSets: slices.Concat(sysmsgThreadArchRules(vars), rules),
		Options: seccomp.ProgramOptions{
			DefaultAction: seccomp.Trap,
			BadArchAction: seccomp.Trap,
		},
	}
}

// SyscallThread returns the program that hands exit_group calls from syscall
// threads to the Sentry.
func SyscallThread(precompiledseccomp.Values) *seccomp.Program {
	return &seccomp.Program{
		RuleSets: []seccomp.RuleSet{{
			Rules: seccomp.MakeSyscallRules(map[uintptr]seccomp.SyscallRule{
				unix.SYS_EXIT_GROUP: seccomp.MatchAll{},
			}),
			Action: seccomp.UserNotify,
		}},
		Options: seccomp.ProgramOptions{
			DefaultAction: seccomp.Allow,
			BadArchAction: seccomp.Allow,
		},
	}
}

// PrecompiledPrograms returns the set of seccomp programs to precompile.
func PrecompiledPrograms() ([]precompiledseccomp.Program, error) {
	var programs []precompiledseccomp.Program
	for _, p := range []struct {
		name string
		vars precompiledseccomp.Values
		fn   func(precompiledseccomp.Values) *seccomp.Program
	}{
		{StubProcessName, StubProcessVars(0, 0), StubProcess},
		{StubProcessWriteTrackingName, StubProcessVars(0, 0), StubProcessWriteTracking},
		{SysmsgThreadName, SysmsgThreadVars(0, 0), SysmsgThread},
		{SyscallThreadName, nil, SyscallThread},
	} {
		program, err := precompiledseccomp.Precompile(p.name, slices.Collect(maps.Keys(p.vars)), p.fn)
		if err != nil {
			return nil, fmt.Errorf("cannot precompile seccomp program %q: %w", p.name, err)
		}
		programs = append(programs, program)
	}
	return programs, nil
}
