# Dirty Tracking with Userfaultfd Write-Protection

Status as of 2026-10-10: Implemented; selected by `--dirty-tracking=auto` on
KVM without application huge pages, opt-in (`--dirty-tracking=uffd`) on
systrap.

## Synopsis

A second source for [dirty page tracking](dirty_tracking.md), `uffd`: the host
kernel records the application's writes to MemoryFiles in page tables, with
userfaultfd write-protection in asynchronous mode, and the Sentry reads them
with `PAGEMAP_SCAN`. A first write after a checkpoint costs a write fault that
the host resolves by itself, instead of a round trip to the Sentry, and reading
the writes costs time in proportion to what was written.

Goals:

-   First writes at the cost of a host page fault, at 4 KiB, so that pre-copy
    converges on workloads that write fast.
-   The same completeness as the `wp` source, proven by the same verification
    (`--dirty-tracking-verify=hash`) and negative controls.
-   Correct on the host kernels in use, including those that lack the 2026
    fixes to `PAGEMAP_SCAN`.
-   On systrap, the narrowest widening of the seccomp filters that can work,
    each grant documented and tested.

Non-goals:

-   Replacing the MemoryFile-level marks: decommits and writes through a
    MemoryFile's file descriptor change no page table.
-   Kernels before Linux 6.7, which keep `wp`.

## Background

Linux 6.7 added two features that make write-protection a cheap write log:

-   **Asynchronous write-protection** (`UFFD_FEATURE_WP_ASYNC`): a range
    registered with a userfaultfd for write-protection
    (`UFFDIO_REGISTER_MODE_WP`) and write-protected (`UFFDIO_WRITEPROTECT`)
    takes a fault at the first write to each page, which the kernel resolves
    itself by clearing the page's write-protect bit, with no message to any
    reader. The bit then records "written since armed". With
    `UFFD_FEATURE_WP_UNPOPULATED`, pages not yet faulted in are protected too,
    by a marker in their page table entry.
-   **`PAGEMAP_SCAN`**, an ioctl on `/proc/PID/pagemap`, reports the pages of a
    range that match a category (`PAGE_IS_WRITTEN`) and, with
    `PM_SCAN_WP_MATCHING`, write-protects them again in the same page table
    walk, so that no write between the report and the re-arming is lost.

The behaviour the design relies on is checked by `//test/hostmm:uffd_wp_test`
on the kernels the tests run on:

-   A userfaultfd tracks the address space (mm) of the process that created
    it, whichever process uses it; marks are per mm, so a page of a shared
    memfd is written if it is written through any of its mappings.
-   Writes made by the kernel on the process's behalf (`read(2)` into a
    buffer) are recorded, also with `UFFD_USER_MODE_ONLY`, which only
    concerns faults delivered to a reader, and asynchronous mode delivers
    none.
-   Writes through the file descriptor (`pwrite(2)`) are not recorded.
-   Zapping page tables (`MADV_DONTNEED`, punching a hole in the memfd) keeps
    both the written state and the protection.
-   Unmapping loses both: a new mapping at the same address is not
    registered, and a scan with `PM_SCAN_CHECK_WPASYNC` fails on it.
-   Without `CAP_SYS_PTRACE` in the initial user namespace, as in runsc's
    sandbox, `userfaultfd(2)` requires `UFFD_USER_MODE_ONLY` where
    `vm.unprivileged_userfaultfd` is 0, the default.

Two kernel bugs shape the harvest. Without commit 07b4377bdbe7 ("fix
PAGEMAP_SCAN written state for unpopulated ptes"), the scan's generic path
reports a written page whose page table entry was zapped as clean; without
40de8160ca7f, it skips zapped huge PMDs. Ubuntu's 6.8 has neither. The fast
path, taken when the category and return masks are both `PAGE_IS_WRITTEN` and
no other mask is set, is correct with and without them, so every harvest uses
exactly those masks.

## Design

### Host helpers

`pkg/sentry/hostmm` holds the ABI-level helpers: `NewWPAsyncUserfaultfd`
creates a userfaultfd with `O_CLOEXEC | O_NONBLOCK | UFFD_USER_MODE_ONLY` and
performs the `UFFDIO_API` handshake for the two features;
`WriteProtectRange` registers and write-protects a range;
`PagemapScanBuf.HarvestWritten` scans a range with the fast-path masks,
`PM_SCAN_WP_MATCHING | PM_SCAN_CHECK_WPASYNC`, resuming at `walk_end` when its
vector fills. `WriteTrackingSyscallRules` returns the seccomp rules of those
ioctls.

### The Sentry's internal mappings

Some writes reach MemoryFile pages through the Sentry's internal mappings
without a MemoryFile-level mark: the Sentry's writes to application memory
through the mappings that the memory manager caches
(`MapInternalUntracked`) and, on KVM, the application's stores. So with
`uffd`, every platform tracks the writes through the internal mappings of
tracked MemoryFiles (`pkg/sentry/pgalloc/write_tracking.go`): runsc creates
the Sentry's userfaultfd and opens its `/proc/self/pagemap` at boot, before
any MemoryFile exists and before seccomp is installed
(`pgalloc.EnableInternalWriteTracking`). `ArmInternalWrites` write-protects a
MemoryFile's chunks, chunks added later are write-protected as they are mapped
(before they become usable), and `HarvestInternalWrites` marks the written
pages dirty, one scan per 1 GiB chunk. A chunk that could not be armed or
scanned is reported written whole.

### Async page loading

Write-protection sees every write through the internal mappings, the Sentry's
own included. A restore with `--background` starts tracking while async page
loading still writes the image's pages into the MemoryFile: through the
chunks' mappings, every loaded page would read as written, and the next
incremental checkpoint would hold the whole image again. So async page loading
writes through a second, unregistered `MAP_SHARED` mapping of the MemoryFile's
file, as the kernel selftests' two-mapping harness does
(`uffd-common.c`), and unmaps it, outside the loader's locks, when loading from
every layer has completed.

### KVM

On KVM, guest-physical memory is the Sentry's address space, so the
application's stores go through the Sentry's mappings, and the internal
mappings' tracking covers them (`platform.WriteTrackingInternalMappings`):
`PAGEMAP_SCAN`'s write-protection issues an MMU notifier invalidation
(`MMU_NOTIFY_PROTECTION_VMA`), KVM drops its writable second-level entries, and
the guest's next store takes an EPT violation that KVM resolves with a host
write fault, which asynchronous write-protection records. No stub, no procfs
and no stub filter are involved; the Sentry's filter gains only the ioctls.
`pkg/sentry/platform/kvm`'s `TestWriteTracking` checks it on a KVM host: guest
stores through write-protected Sentry mappings are reported, and harvests
re-arm them.

### systrap

On systrap, the application writes through the stubs' mappings, one mm per
stub; the platform tracks them itself (`platform.WriteTrackingPlatform`,
`pkg/sentry/platform/systrap/write_tracking.go`) and implements
`platform.WriteTracker`.

-   **Userfaultfds.** A userfaultfd tracks the mm of the process that creates
    it, so each stub creates its own, by a `userfaultfd(2)` that the Sentry
    injects before the stub's first mapping of a MemoryFile. Stubs share the
    Sentry's FD table, so the Sentry then uses it directly: the handshake,
    registration and write-protection act on the stub's mm.
-   **Pagemaps.** The Sentry opens each stub's `/proc/PID/pagemap` itself,
    through a directory FD of procfs that runsc opens at boot (see below).
-   **Mapping.** A stub's mapping of a tracked MemoryFile is created without
    write access, registered and write-protected, then given its protection
    with `mprotect(2)`, so that no write precedes the protection: a concurrent
    write faults into the Sentry, which retries it once the mapping is
    complete. Mappings of MemoryFiles that become tracked later are
    write-protected by `ArmWrites`, with the kernel paused.
-   **Harvests.** `HarvestWrites` scans every mapping of every stub. A mapping
    about to be unmapped or replaced is harvested first, since unmapping
    loses what was recorded; it is made inaccessible (`PROT_NONE`) before
    that harvest, so that no write lands between the harvest and the
    unmapping. Each stub keeps a sorted table of its MemoryFile mappings for
    this.
-   **Dead stubs.** The mm of a dead process has no page tables to scan, so the
    mappings of a stub that died are reported written whole.

### The procfs handle

runsc unmounts `/proc` from the sandbox before installing seccomp, and the
sandbox must not be able to open files by path. The Sentry nevertheless needs
the pagemaps of stubs that it creates later, so runsc opens a directory FD of
the root of the procfs that `/proc/self` is in (`/proc/self/..`; in the
sandbox's chroot, `/proc` is only a link to it) before seccomp, keeps it for
the sandbox's lifetime, and exempts it from the check that no directory FD
stays open. The Sentry, which holds `CAP_SYS_PTRACE` in the sandbox's user
namespace, opens `PID/pagemap` through it with `openat(2)`.

In a sandbox with its own PID namespace, that procfs shows the stubs under
their PIDs in the Sentry's namespace. Where it is of an ancestor PID
namespace, as when tests run the sandbox without its own root, the Sentry
finds a stub's PID there in the `Pid:` line of the fdinfo of a pidfd of the
stub.

The plan before this design kept `/proc` mounted for a stub-side `openat(2)`
of `/proc/self/pagemap`. The stub filter allows `mmap(2)` and `munmap(2)` with
any arguments, and hosts without `mseal(2)` (Ubuntu's 6.8) cannot seal the
stub's code, so the application could redirect the path that a stub-side
`openat(2)` reads from stub memory to any file. Opening the pagemaps in the
Sentry, through a handle taken before the sandbox is locked down, keeps
`/proc` unreachable by path and keeps `openat(2)` out of the stub filter.

### The dirty source

`kernel.NewUffdDirtySource` reports the internal mappings' writes and, on
systrap, the platform's: `Arm` arms what is not armed yet (MemoryFiles that
became tracked and their mappings; harvests re-arm what they report), and
`Harvest` harvests both. Pre-copy throttling (`--precopy-throttle=on`) delays
tasks at the faults that disarm `wp`'s write-protection; `uffd` records writes
without any fault into the Sentry, so a pre-copy that may throttle is refused
under `uffd` rather than run unthrottled.

## Seccomp grants

Each grant is added only when the source is enabled, and is the narrowest
that the code needs.

Who    | System call                                                                   | Platform                          | Why
------ | ----------------------------------------------------------------------------- | --------------------------------- | ---
Sentry | `ioctl` `UFFDIO_API`, `UFFDIO_REGISTER`, `UFFDIO_WRITEPROTECT`, `PAGEMAP_SCAN` | KVM, systrap                      | set up, arm and harvest write tracking
Sentry | `openat(procfs FD, *, O_RDONLY \| O_CLOEXEC)`                                  | systrap                           | open the stubs' pagemaps
Sentry | `pidfd_open(*, 0)`                                                            | systrap, procfs of an ancestor ns | find a stub's PID in that procfs
Stub   | `userfaultfd(O_CLOEXEC \| O_NONBLOCK \| UFFD_USER_MODE_ONLY)`                  | systrap                           | create the userfaultfd of the stub's mm
Stub   | `mprotect(*, *, PROT_NONE or a protection with PROT_WRITE)`                    | systrap                           | make a tracked mapping writable once protected, inaccessible before its last harvest

Application code cannot make the stubs' system calls: it runs in stub threads
whose own seccomp filter traps every system call to the Sentry; only the
stubs' syscall threads, which run what the Sentry injects, can. The stub
filter is a second line, which bounds what code running in a stub outside
the Sentry's control could do.

-   **The ioctls** act only on userfaultfds and pagemap files, which the
    Sentry holds for its own mm and its stubs'; the Sentry cannot create a
    userfaultfd itself after seccomp. The command numbers are checked; the
    file descriptor is not, as for the Sentry's other ioctls.
-   **`openat(2)` on the procfs FD** allows read-only opens of any file of that
    procfs, and of absolute paths in the Sentry's root (an empty, read-only
    directory): seccomp cannot check paths. In a sandbox with its own PID
    namespace, the procfs shows only the Sentry and its stubs, so what it
    exposes is information the Sentry already has (their memory, maps and
    status), what procfs shows any unprivileged reader (`/proc/sys`, for
    example), and the reopening, read-only, of files that the Sentry holds
    open (`/proc/self/fd/N`), which its own access to them already covers.
-   **`pidfd_open(2)`** is a deliberate extension of the design accepted on
    2026-10-10 ("the stub filter gains only `userfaultfd`"): it is needed only
    where the procfs is of an ancestor PID namespace, and is allowed only
    there; runsc's production sandboxes have their own PID namespace and never
    get it. A pidfd only allows what the filter also allows on it, reading
    its fdinfo here (`pidfd_send_signal(2)` and `pidfd_getfd(2)` stay denied).
-   **`userfaultfd(2)`** is allowed with exactly the flags write tracking uses.
    `UFFD_USER_MODE_ONLY` makes the kernel never deliver a fault that it takes
    itself, on behalf of a system call, to a reader: stalling the kernel at a
    chosen point in a system call is what makes userfaultfds an aid to kernel
    exploits, and why `vm.unprivileged_userfaultfd` defaults to 0. The new
    userfaultfd tracks the stub's own mm only.
-   **`mprotect(2)`** is the other deliberate extension of the accepted
    design. It is needed because a tracked mapping must not be writable
    before it is write-protected (a write in between would be lost) and must
    be inaccessible before its last harvest (a write after it would be lost).
    It is allowed only with `PROT_NONE` and the four protections that include
    `PROT_WRITE`, the protections that `MapFile` gives writable mappings;
    making memory readable or executable without write access is refused.
    The stub filter already allows `mmap(2)` with any arguments, which can
    replace a mapping of a file with any protection, so the restriction
    reduces the grant to what the code uses rather than closing a capability
    that `mmap(2)` leaves open.

`TestStubSeccompFilterWriteTracking` runs the stubs' seccomp program on each
of these system calls and on the protections and flags it must refuse, and
`TestSeccompInfoWriteTracking` checks the Sentry's rules.

## Choosing the source

Measured on systrap (Ubuntu 6.8, 4 vCPUs), with experiment 02's first-write
program over 256 MiB, each epoch ended by a `checkpoint --leave-running`,
medians of 3:

Source           | First write, per page | Checkpoint pause, all written / none
---------------- | --------------------- | ------------------------------------
untracked        | 19 ns                 | 0.27 s / 0.26 s
`uffd`           | 1.42 µs               | 0.32 s / 0.29 s
`wp`, 64 KiB     | 3.1 µs                | 0.27 s / 0.26 s
`wp`, 4 KiB      | 26.7 µs               | 0.36 s / 0.28 s

The harvest in the pause took 2.2 ms with nothing written and up to 16 ms
after 256 MiB were; the pauses otherwise differ within the host's noise.
Harvesting a 1 GiB chunk of internal mappings takes 2.9 ms with nothing
written and 3.5 ms after 64 MiB were (`BenchmarkHarvestInternalWrites`). On
1 GiB of shared memory, a write-protect fault costs 1.2 µs per page, arming
6.5–7 ms, and a harvest with re-arming 0.4, 0.5, 1.6 and 11 ms with 0, 1, 10
and 100 % written. Under pre-copy (50 MiB/s of random writes, a 100 MiB/s
store), `uffd` converged in 6 rounds with an 83 ms final pause and an 84 ms
longest stall; `wp` at 4 KiB in 4 rounds with a 140 ms pause but a 248 ms
stall while re-arming; `wp` at 64 KiB did not converge.

`--dirty-tracking=auto` selects `uffd` on KVM, where the source only adds
the Sentry's ioctls, if the host supports it and application huge pages are
disabled (`--app-huge-pages=false`), and `wp` otherwise. On systrap, `uffd` is
opt-in: it is faster, but it widens the Sentry's and the stubs' filters as
above, a trade that the operator makes. KVM's costs were not measured: no
KVM host was available to the bench; CI checks its correctness.

## Requirements and caveats

-   Linux 6.7 or later, built with userfaultfd write-protection
    (`CONFIG_PTE_MARKER_UFFD_WP`). runsc probes the features at boot: `uffd`
    fails the sandbox's start where they are missing, and `auto` falls back
    to `wp`.
-   Write-protection splits huge pages: arming zaps PMD mappings, which fault
    back at 4 KiB (166–348 µs per MiB). `auto` therefore keeps `wp` when
    application huge pages are enabled.
-   Arming populates page tables for the whole tracked memory: about 2 MiB per
    GiB, per mm, so per stub mapping it on systrap.
-   Every harvest walks every armed mapping: about 3 ms per GiB with little
    written, on top of what was.

## Tests

-   `//test/hostmm:uffd_wp_test` checks the host semantics above, with stubs
    in the same and in a new user namespace.
-   hostmm, pgalloc, systrap and kvm unit tests check each part: user and
    kernel-mode writes, zaps, holes, re-arming, chunks added after arming,
    pages loaded asynchronously after arming, stub stores before an unmapping
    or a replacement, mappings armed late, dead stubs, guest stores on KVM.
-   The syscall tests' `_save_verify_uffd` and `_save_verify_uffd_incremental`
    variants run every test with `--dirty-tracking=uffd` and verification on
    KVM and systrap; runsc/container's `TestCheckpointRestorePrecopy` and
    `TestCheckpointRestoreBackgroundUffd` (a restore with `--background`, then
    an incremental checkpoint that must not hold the loaded pages) run on both
    platforms, as do the incremental checkpoints' no-escapes, mem-touch and
    pre-copy churn tests.
-   Negative controls: `--TESTONLY-dirty-tracking-break=uffd-internal`
    disables the internal mappings' marks, `uffd-unmap` those of the harvest
    before a stub mapping is removed; the package tests check that each loses
    the writes it marks, and the container tests that verification and the
    comparison with full checkpoints find what is lost.
-   Tests that need write tracking skip where the host lacks it, and fail
    instead with `GVISOR_REQUIRE_UFFD_WP=1`, which CI sets so that a skipped
    test cannot pass for one that ran.

## Alternatives considered

-   **Keeping `/proc` mounted** for a stub-side `openat(2)`: rejected above.
-   **A helper outside the sandbox** passing pagemap FDs: one more process
    and protocol, for what a handle taken at boot gives.
-   **`pidfd_getfd(2)`** to take each stub's userfaultfd: unnecessary, since
    stubs share the Sentry's FD table.
-   **Soft-dirty** loses writes across zaps and resets a whole mm under its
    write lock (7 ms per GiB mapped) whatever was written.
-   **A KVM dirty log** sees only guest stores, and exists only on KVM.

## Prior art

-   `PAGEMAP_SCAN` and asynchronous write-protection were added for Wine's
    emulation of Windows' `GetWriteWatch`, and CRIU reads page state with
    `PAGEMAP_SCAN`; the kernel selftests (`pagemap_ioctl.c`,
    `uffd-unit-tests.c`) are their reference users.
-   QEMU's background snapshots write-protect guest memory with userfaultfd,
    in synchronous mode.
