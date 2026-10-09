// Copyright 2019 The gVisor Authors.
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

package systrap

import (
	"os"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/seccomp"
	"gvisor.dev/gvisor/pkg/seccomp/precompiledseccomp"
	"gvisor.dev/gvisor/pkg/sentry/hostmm"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

// sysmsgThreadPriorityVarName is the seccomp filter variable name used to
// encode the sysmsg thread priority.
const sysmsgThreadPriorityVarName = "systrap_sysmsg_thread_priority"

// writeTrackingProcFDVarName is the seccomp filter variable name used to
// encode the procfs directory FD of write tracking.
const writeTrackingProcFDVarName = "systrap_write_tracking_proc_fd"

// systrapSeccomp implements platform.SeccompInfo.
type systrapSeccomp struct {
	// trackWrites is true if the platform tracks writes.
	trackWrites bool
}

// Variables implements `platform.SeccompInfo.Variables`.
func (s systrapSeccomp) Variables() precompiledseccomp.Values {
	initSysmsgThreadPriority()
	vars := precompiledseccomp.Values{}
	vars.SetUint64(sysmsgThreadPriorityVarName, uint64(sysmsgThreadPriority))
	if s.trackWrites {
		vars[writeTrackingProcFDVarName] = uint32(writeTracking.procFD)
	}
	return vars
}

// ConfigKey implements `platform.SeccompInfo.ConfigKey`.
func (s systrapSeccomp) ConfigKey() string {
	if s.trackWrites {
		return "systrap-write-tracking"
	}
	return "systrap"
}

// SyscallFilters implements `platform.SeccompInfo.SyscallFilters`.
func (s systrapSeccomp) SyscallFilters(vars precompiledseccomp.Values) seccomp.SyscallRules {
	rules := s.baseSyscallFilters(vars)
	if s.trackWrites {
		// The Sentry opens the pagemap of each stub through the procfs
		// FD, then tracks writes with the stub's userfaultfd and pagemap.
		// The path cannot be checked: the rule allows read-only opens of
		// any file of that procfs, and absolute paths in the Sentry's
		// root.
		rules.Merge(seccomp.MakeSyscallRules(map[uintptr]seccomp.SyscallRule{
			unix.SYS_OPENAT: seccomp.PerArg{
				seccomp.EqualTo(vars[writeTrackingProcFDVarName]),
				seccomp.AnyValue{},
				seccomp.EqualTo(unix.O_RDONLY | unix.O_CLOEXEC),
			},
			// To find a stub's PID in a procfs of an ancestor PID
			// namespace; see procPID.
			unix.SYS_PIDFD_OPEN: seccomp.PerArg{
				seccomp.AnyValue{},
				seccomp.EqualTo(0),
			},
		}))
		rules.Merge(hostmm.WriteTrackingSyscallRules())
	}
	return rules
}

// baseSyscallFilters returns the syscall rules of the platform without write
// tracking.
func (systrapSeccomp) baseSyscallFilters(vars precompiledseccomp.Values) seccomp.SyscallRules {
	return seccomp.MakeSyscallRules(map[uintptr]seccomp.SyscallRule{
		unix.SYS_PTRACE: seccomp.Or{
			seccomp.PerArg{
				seccomp.EqualTo(unix.PTRACE_ATTACH),
			},
			seccomp.PerArg{
				seccomp.EqualTo(unix.PTRACE_CONT),
				seccomp.AnyValue{},
				seccomp.EqualTo(0),
				seccomp.EqualTo(0),
			},
			seccomp.PerArg{
				seccomp.EqualTo(unix.PTRACE_GETEVENTMSG),
			},
			seccomp.PerArg{
				seccomp.EqualTo(unix.PTRACE_GETREGSET),
				seccomp.AnyValue{},
				seccomp.EqualTo(linux.NT_PRSTATUS),
			},
			seccomp.PerArg{
				seccomp.EqualTo(unix.PTRACE_GETSIGINFO),
			},
			seccomp.PerArg{
				seccomp.EqualTo(unix.PTRACE_SETOPTIONS),
				seccomp.AnyValue{},
				seccomp.EqualTo(0),
				seccomp.EqualTo(unix.PTRACE_O_TRACESYSGOOD | unix.PTRACE_O_TRACEEXIT | unix.PTRACE_O_EXITKILL),
			},
			seccomp.PerArg{
				seccomp.EqualTo(unix.PTRACE_SETREGSET),
				seccomp.AnyValue{},
				seccomp.EqualTo(linux.NT_PRSTATUS),
			},
			seccomp.PerArg{
				seccomp.EqualTo(linux.PTRACE_SETSIGMASK),
				seccomp.AnyValue{},
				seccomp.EqualTo(8),
			},
			seccomp.PerArg{
				seccomp.EqualTo(unix.PTRACE_SYSEMU),
				seccomp.AnyValue{},
				seccomp.EqualTo(0),
				seccomp.EqualTo(0),
			},
			seccomp.PerArg{
				seccomp.EqualTo(unix.PTRACE_DETACH),
			},
		},
		unix.SYS_TGKILL: seccomp.MatchAll{},
		unix.SYS_RT_TGSIGQUEUEINFO: seccomp.PerArg{
			seccomp.EqualTo(os.Getpid()),
			seccomp.AnyValue{}, // tid
			seccomp.EqualTo(unix.SIGKILL),
		},
		unix.SYS_WAIT4: seccomp.MatchAll{},
		unix.SYS_IOCTL: seccomp.Or{
			seccomp.PerArg{
				seccomp.NonNegativeFD{},
				seccomp.EqualTo(linux.SECCOMP_IOCTL_NOTIF_RECV),
			},
			seccomp.PerArg{
				seccomp.NonNegativeFD{},
				seccomp.EqualTo(linux.SECCOMP_IOCTL_NOTIF_SEND),
			},
			seccomp.PerArg{
				seccomp.NonNegativeFD{},
				seccomp.EqualTo(linux.SECCOMP_IOCTL_NOTIF_SET_FLAGS),
				seccomp.EqualTo(linux.SECCOMP_USER_NOTIF_FD_SYNC_WAKE_UP),
			},
		},
		unix.SYS_WAITID: seccomp.PerArg{
			seccomp.EqualTo(unix.P_PID),
			seccomp.AnyValue{},
			seccomp.AnyValue{},
			seccomp.EqualTo(unix.WEXITED | unix.WNOHANG | unix.WNOWAIT),
		},
		unix.SYS_SETPRIORITY: seccomp.PerArg{
			seccomp.EqualTo(unix.PRIO_PROCESS),
			seccomp.AnyValue{},
			seccomp.EqualTo(vars.GetUint64(sysmsgThreadPriorityVarName)),
		},
	}).Merge(archSyscallFilters())
}

// HottestSyscalls implements `platform.SeccompInfo.HottestSyscalls`.
func (systrapSeccomp) HottestSyscalls() []uintptr {
	return hottestSyscalls()
}

// SeccompInfo returns seccomp filter info for the systrap platform.
func (p *Systrap) SeccompInfo() platform.SeccompInfo {
	if p == nil {
		return systrapSeccomp{}
	}
	return systrapSeccomp{trackWrites: p.trackWrites}
}

// PrecompiledSeccompInfo implements
// platform.Constructor.PrecompiledSeccompInfo.
func (*constructor) PrecompiledSeccompInfo() []platform.SeccompInfo {
	return []platform.SeccompInfo{(*Systrap)(nil).SeccompInfo()}
}
