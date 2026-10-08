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
