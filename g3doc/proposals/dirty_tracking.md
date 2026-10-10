# Dirty Page Tracking

Status as of 2026-10-09: Implemented; off by default.

## Synopsis

Record, for every savable `pgalloc.MemoryFile`, the set of pages written since
a point in time, so that a checkpoint can save only the pages changed since the
image the sandbox was last saved to or restored from (incremental checkpoints),
and so that memory can be copied while the sandbox runs and then only the pages
written meanwhile (pre-copy).

Goals:

-   Completeness: a page whose contents change must be in the dirty set,
    whoever changes it (the application, the Sentry, the host kernel on the
    Sentry's behalf), and a test mode must prove it.

-   No cost when tracking is off, and no host feature, privilege or platform
    support required to turn it on.

-   One mechanism for every source of writes: the bitmap and the
    MemoryFile-level marks are shared, and the sources that report application
    stores are pluggable.

Non-goals:

-   Making the checkpoint's object graph incremental: it is always saved whole.

## Background

gVisor does not track writes. The Sentry learns which pages of a MemoryFile are
committed from `mincore(2)` and a zero scan when it saves them, and
`_KVM_MEM_LOG_DIRTY_PAGES` is defined in the kvm platform but unused. Writes
reach a savable MemoryFile's pages by many paths:

Path                                                              | Example                                                               | Seen by page tables
----------------------------------------------------------------- | --------------------------------------------------------------------- | -------------------
Application stores through the platform's mappings                | any write to `mmap`ed memory                                          | the application's
Sentry writes through mm's cached internal mappings               | `CopyOut`, `read(2)` into a buffer, futex words                       | the Sentry's
`MapInternal` with write access                                   | copy-on-write copies, page cache fills, tmpfs data, the VDSO page     | the Sentry's
Long-lived writable internal mappings                             | io_uring's rings                                                      | the Sentry's
Writes through the MemoryFile's file descriptor                   | tmpfs data on a disk-backed filestore (`pwritev2(2)`)                 | none
Content changes without any write                                 | decommit and release (`fallocate(FALLOC_FL_PUNCH_HOLE)`)              | none

Mechanisms based on page tables (soft-dirty, userfaultfd write-protection, a
KVM dirty log) see only some of these, and only in the address spaces they
watch: none sees the last two rows. So every design needs marks at the
MemoryFile level, and the sources that watch page tables are added to them.

## Design

The MemoryFile side is in `pkg/sentry/pgalloc/dirty.go`; the kernel side, in
`pkg/sentry/kernel/dirty.go`.

### The bitmap

A tracked MemoryFile keeps one bit per page, in blocks of 32 KiB per 1 GiB
chunk of the file (32 KiB per GiB), held as an atomic pointer to a slice of
blocks. The slice grows with the file's chunks, under the MemoryFile's mutex
and before the new chunks become usable, so marks, which are lock-free atomic
ORs of whole words, never race with growth.

-   `EnableDirtyTracking` starts tracking; `DirtyTracked` reports it.
-   `MarkDirty(fr)` marks a range.
-   `SwapDirty(paused)` returns the pages dirtied since the previous swap and
    clears them, word by word with atomic swaps, so that marks racing with a
    swap land in either the returned set or the next one.
-   `UnswapDirty(s)` folds a swapped set back when the save that consumed it
    fails, so that a failed save loses no dirty page: Firecracker's rule
    (`vstate/memory.rs`, `mark_dirty`) and test
    (`test_snapshot_not_losing_dirty_pages.py`).

### MemoryFile-level marks

The MemoryFile marks the writes it sees:

-   `MapInternal` with write access marks the range before returning the
    mappings. This covers copy-on-write copies, page cache fills, tmpfs data in
    application memory, the VDSO parameter page (which the timekeeper rewrites
    on its own), and the zeroing of recycled pages, which `Allocate` writes
    through `MapInternal`.
-   Decommit and release mark the pages they zero, as does the manual zeroing
    that replaces a failed decommit.

Writers that bypass both call `MarkDirty`: tmpfs, which writes file data to
disk-backed MemoryFiles with `pwritev2(2)` on the MemoryFile's file descriptor,
and dirty sources (below). Async page loading marks nothing: it restores the
contents of the image being loaded, which is the image the next save is
relative to.

### The carry rule

A `MapInternal` mark precedes its write, and callers keep the mappings it
returns without telling the MemoryFile when they are done with them. A swap
that happens while tasks run may therefore come between a mark and its write.
So the pages marked through `MapInternal` since the previous swap are reported
again by the next swap, unless the swap happened with the kernel paused
(`SwapDirty(paused=true)`): a second bitmap holds the `MapInternal` marks of
the current epoch for this purpose.

### Always-dirty ranges

A writer that keeps an internal mapping for a long time and writes through it
without calling `MapInternal` again registers the range with
`MarkAlwaysDirty(fr)`, and the range is reported by every swap until
`ClearAlwaysDirty(fr)`. Registrations are counted. io_uring's rings are the
only such mappings: the Sentry writes completions through mappings it obtained
once. Other rings shared with the application need no registration:
packet-mmap rings and kcov's coverage area call `MapInternal` for each access,
and the Sentry never writes to an AIO ring, since `io_getevents(2)` copies
events out through the MemoryManager like any other write to application
memory.

### Dirty sources and epochs

A `kernel.DirtySource` reports the writes that MemoryFiles cannot see,
application stores through page tables:

```go
type DirtySource interface {
    Name() string
    // Arm makes the next write to every tracked page observable.
    Arm(ctx context.Context, paused bool) error
    // Harvest moves the writes observed since the last Arm or Harvest into
    // the MemoryFiles' dirty sets.
    Harvest(ctx context.Context) error
}
```

Tracking is divided into epochs. `Kernel.DirtyEpoch(ctx, paused)` ends one: it
harvests every source, swaps the dirty set of every tracked MemoryFile and
re-arms every source, returning the sets; if arming fails, the sets are folded
back. Saves end an epoch inside their pause; pre-copy rounds end one while
tasks run.

Tracking is configured with `Kernel.SetDirtyTracking(DirtyTrackingOpts)`, and
starts at the end of every successful save and at every restore, on the
MemoryFiles saved or loaded, the private ones (filestores) included: that image
is the parent of the next incremental save. A restore tracks the application
MemoryFile before the timekeeper rewrites the VDSO parameter page. A save ends
the current epoch inside its pause and, if it fails, folds its sets back.

### Verification

Tracking is only as complete as its marks and sources, so a debugging mode
checks it on every save. At the start of an epoch (`RecordPageHashes`), every
page of every tracked MemoryFile is hashed (8 bytes per page; the hashes never
leave the process, so `hash/maphash` serves); at the end
(`VerifyDirty(s)`), every page is hashed again, and each page that changed
without being in the swapped set is an *escape*. With
`DirtyTrackingOpts.Verify`, the kernel logs the first escapes, counts them in
the metric `/checkpoint/dirty_tracking_escapes`, and fails the save. Restores
then wait for every page to be loaded, since every page is hashed.

### Negative controls

Verification is only useful if a missing mark would not go unnoticed. Each
marking path is identified by a `pgalloc.DirtyMarkPath`, and
`pgalloc.TestOnlyDisableDirtyMarkPath(p)` disables one: tests check, for every
path, that verification reports the writes it marks as escapes when it is
disabled, and none when it is enabled. Disabling costs nothing when tracking
is off: `MapInternal` and `MarkDirtyBy` read the disabled path, one atomic
load, only for tracked MemoryFiles.

## Dirty source: Sentry write-protection

The first source, `kernel.WriteProtectDirtySource` ("wp", in
`pkg/sentry/mm/dirty.go`), needs nothing from the platform or the host: gVisor
already intercepts every application fault in the Sentry, and `mm.Fork`
already arms copy-on-write by withholding write permission from pmas and
unmapping them. wp does the same to record first writes.

### Arming

`MemoryManager.ArmDirtyTracking(unit)`, called by the source's `Arm` for every
MemoryManager (the kernel is paused so that it reaches those of tasks in the
middle of `execve`), sets `pma.dirtyArmed` on every pma that maps a tracked
MemoryFile, private and shared alike, and withholds Write from its
`effectivePerms` and `maxPerms`. The pmas that were writable are unmapped from
the AddressSpace once per vma, spanning the pmas between them, rather than once
per pma: after an epoch every written unit is a pma of its own, and unmapping
each costs a host syscall on systrap and an invalidation on kvm (one unmap per
pma held the MemoryManager's lock for up to 286 ms at a 4 KiB unit with tens of
thousands of written units). The pmas that the previous epoch split are merged
back; the merge rule compares `dirtyArmed`.

`dirtyArmed` is distinct from `needCOW`, whose semantics include copying, and
is saved with the permissions it withholds: a pma restored without it would
keep Write withheld with nothing to restore it.

### First writes

A write to an armed pma, by the application (a fault) or by the Sentry on its
behalf (`CopyOut` falls off `existingPMAsLocked`'s fast path, which checks
`effectivePerms`), reaches `getPMAsInternalLocked`. There, the pma is split
around the *tracking units* that the write overlaps, which are marked dirty
in the MemoryFile and made writable again from the vma's permissions; the
write proceeds as usual. Units are aligned to the unit size, 64 KiB by
default, and to the huge page for huge pmas.

Every path that grants Write respects the flag: new pmas of a tracked
MemoryFile are created armed (and revisited, so that the write that created
them is recorded); copy-on-write copies are marked by `MapInternal` and start
disarmed; taking ownership of a copy-on-write page without copying keeps the
pma armed; `mprotect(2)` withholds Write from armed pmas; `mremap(2)` moves
pmas with their state. mm's cached internal mappings come from
`MapInternalUntracked`, so that obtaining them neither marks whole pmas nor
carries them into the next epoch: the permission check tracks the writes
through them.

### Units and cost

A first write on systrap costs a stub fault, a round trip to the Sentry, an
`mmap(2)` injected into the stub, and the host faulting the unit's pages in
again (about 1.5 µs each). Larger units fault less often per page and record
more pages; saves that compare page hashes with the parent image's refine
dirty units back to the pages that changed, which makes a 64 KiB unit as
precise as a 4 KiB one in practice.

Measured on systrap (4 shared vCPUs, Linux 6.8), with 256 MiB written at
random after a checkpoint, median of 3; the prototype's numbers in
parentheses:

Unit   | First write, per page | Arming 256 MiB, all written (pause included)
------ | --------------------- | --------------------------------------------
4 KiB  | 26.1 µs (22–27)       | 20–21 ms
64 KiB | 3.9 µs (3.7–4.1)      | 11–24 ms
2 MiB  | 1.7 µs (1.7)          | —

An untracked write costs 21 ns. On the prototype, a Python step building a
dataframe (0.22 s untracked) took 0.71 s at 4 KiB and 0.31 s at 64 KiB, and a small edit
whose content diff is 1.9 MiB recorded 3.7 MiB at 4 KiB, 21.5 MiB at 64 KiB,
and 2.0 MiB at 64 KiB with hash refinement. A program rewriting 128 MiB/s pays
33 ms per 12.8 MiB at 64 KiB: acceptable between checkpoints, and the reason
for userfaultfd write-protection, a host-level source, under pre-copy.

### On kvm

Nothing is platform-specific: a first write is a guest page fault, the same
`getPMAsInternalLocked` path, and a guest page table update, with no host
`mmap(2)` and no host re-fault, so it should cost less than on systrap. kvm's
`MapUnit` is 16 MiB, but an isolated unit's pma bounds what a fault maps.
Arming invalidates the guest TLBs once per unmapped range, so coalescing
unmaps matters there too. kvm's `AddressSpace.MapFile` obtains the host
addresses it maps into the guest with `MapInternalUntracked`: with
`MapInternal`, mapping a disarmed unit writable would mark it again and, when
the epoch ends while tasks run (pre-copy), carry it into the next epoch, so
that every round of a pre-copy would find more to write than was written.

### Configuration

-   `--dirty-tracking=off|auto|wp` (default `off`): `wp` selects this source;
    `auto` selects the best available.
-   `--dirty-tracking-unit` (default 64 KiB): the tracking unit, a power of 2
    of at least a page.
-   `--dirty-tracking-verify=off|hash` (default `off`): with `hash`, every
    checkpoint verifies tracking and fails on an escape.
-   `--TESTONLY-dirty-tracking-break=none|mapinternal|decommit|tmpfs|iouring|fault|arm`
    disables one marking path, for container tests that check that
    verification catches it; `fault` and `arm` are this source's first-write
    mark and its arming.

The syscall tests' `_save_verify` variants run every test, on every platform,
with `--dirty-tracking=wp --dirty-tracking-verify=hash` and a save after
every test, and `_save_verify_4k`, on the default platform, at a 4 KiB unit.
An autosave that verification fails ends the sandbox with exit status 1, so
that the test fails: the image is complete, and the test would otherwise go
on from it.

## Alternatives considered

-   **A KVM dirty log** (`KVM_MEM_LOG_DIRTY_PAGES`) sees only guest writes on
    the kvm platform: it misses the Sentry's writes in host mode, the writes
    through the MemoryFile's file descriptor and decommits, and does not exist
    on systrap. It could be one more source later.
-   **Soft-dirty** (`/proc/pid/clear_refs`) is per process and per mm: the
    Sentry would read the pagemap of every systrap stub and of itself, and it
    does not see decommits either. Its reset write-protects the whole mm.
-   **Content hashing alone** (comparing every page with the previous image)
    needs no tracking but reads all memory at every save: it is what
    verification does, at a cost only tests can pay.

## Prior art

-   Firecracker ORs a userspace bitmap, fed by its own writes, with KVM's dirty
    log, and folds the bitmap back when a snapshot fails.
-   QEMU keeps a `DIRTY_MEMORY_MIGRATION` bitmap per RAM block, synchronized
    from each source (KVM's log, its own writes, background-snapshot's
    userfaultfd write-protection).
-   CRIU's pre-dumps and incremental dumps use soft-dirty bits, and its zdtm
    suite compares a pre-dumped chain with a full dump of the same moment.
