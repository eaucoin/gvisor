# Incremental Checkpoints

Status as of 2026-10-10: Implemented; requires dirty tracking, which is off by
default.

## Synopsis

Let a checkpoint save only the memory written since the image the sandbox was
last checkpointed to or restored from, its *parent*, and refer to the parent's
layers for every other page:

```bash
runsc checkpoint --image-path=<image 2> --parent-image-path=<image 1> <container id>
```

Goals:

-   A checkpoint's pause and size follow what the application wrote since the
    previous one, not the size of its memory.
-   A chain of incremental checkpoints restores like one image: every page is
    read from the image that holds it, in one hop, however long the chain.
-   A delta never refers to an image it is not a delta of, and a failed
    checkpoint loses no write.

Non-goals:

-   Making the object graph (`checkpoint.img`) incremental: it is always saved
    whole, 0.2 to 1.1 MB for interpreters and development servers.
-   Choosing when to checkpoint or compact: that is the caller's policy, for
    which this document gives guidance.

This builds on [dirty page tracking](dirty_tracking.md), which records the
pages written since the last checkpoint, and on the
[checkpoint image format](checkpoint_image_format.md), whose images name their
layers by identity. How to use it is described in the
[user guide](../user_guide/checkpoint_restore.md#incremental-checkpoints).

## Background

A full checkpoint pauses the sandbox for its whole memory: about 1.15 ms per
MiB written through the page cache, 0.34 ms with `O_DIRECT`, on top of 30 to
40 ms to quiesce the sandbox and save its object graph. Changing 1 MB of a
1 GiB interpreter rewrote 1.06 GB.

What changes between checkpoints is small once a workload has built its working
set. Measured on interpreters and development servers whose images are 20 to
106 MiB, by diffing the pages of successive full checkpoints:

Workload                       | Step                   | Image (MiB) | Pages changed (MiB)
------------------------------ | ---------------------- | ----------- | -------------------
Python REPL with numpy, pandas | pytest in a subprocess | 88.6        | 1.8
Python REPL with numpy, pandas | small edit             | 88.7        | 1.9
Python REPL with numpy, pandas | tiny exec              | 88.7        | 0.5
Python REPL with numpy, pandas | idle, 1 or 4 minutes   | 88.7        | one page
Bun REPL                       | small edit             | 99.6        | 0.8
Bun REPL                       | tiny exec              | 99.7        | 1.3
Bun REPL                       | idle, per period       | 96.0–96.4   | 13.2–13.7
Bun, after `npm install`       | tiny exec              | 19.7        | 0.9
Vite dev server                | idle, per period       | 39.8–41.0   | 5.0–6.1

A small exec changes 0.5 to 2 MiB, 1 to 2 % of the image. Zero pages are
negligible in deltas (at most 0.16 MiB per step).

## Design

### The parent, through the save path

`runsc checkpoint --parent-image-path=DIR` reads the identity of the image in
`DIR` (the SHA-256 of its `pages_meta.img`, `checkpointimage.FileDigest`) and
passes it as `control.SaveOpts.ParentImageDigest`, which becomes
`state.SaveOpts.Parent` and the `parent` argument of `Kernel.SaveTo`.
`Kernel.saveToLocked` refuses an incremental save without dirty tracking or
without a pages file, ends the dirty tracking epoch inside the pause
(`beginDirtySave`), and starts the incremental save (`beginIncrementalSave`).
`saveMemoryFiles` then saves each MemoryFile as a delta of its part of the
parent (`incrementalSave.setBase`, in `pkg/sentry/kernel/incremental.go`):

-   `pgalloc.SaveOpts.Base` is the parent's `MemoryFileImage` of the same
    MemoryFile: the application MemoryFile's, or the private MemoryFile's of
    the same owner;
-   `BaseLayers` maps the parent's layer *i* to layer *i*+1 of the new image,
    whose layers are the image itself, the parent, then the parent's layers;
-   `Clean(off)` is true for the pages not in the epoch's dirty set.

`MemoryFile.SaveTo` refers each clean page to the extent the parent has for
it, already resolved to the layer that holds its data, and writes the others.
A page that tracking reports written, but whose XXH64 equals the parent's
hash at that offset, is saved as unchanged (*hash refinement*): almost half of
what tracking reports is written back with the same content, and refinement
makes the default 64 KiB tracking unit as precise as a page. Zero pages get no
extent. The metadata of every MemoryFile (allocations, memory accounting) is
always written whole: it is small, and must be exact.

`checkpointimage.Writer.Finish` drops the layers that no extent refers to, so
a chain shortens itself as pages are written over. The image's state file
metadata records the parent's identity as `parent_id`.

### Identity

The Kernel keeps the `checkpointimage.Image` it was last saved to or restored
from (`dirtyTracking.last`), when that image has a pages file: the end of a
successful save sets it, and so does the start of a restore. An incremental
save names its parent by digest, and fails unless it is that image: a delta is
only correct relative to the image whose epoch its dirty set started with, so
that a sandbox restored from image B cannot write a delta "of" image A, and a
second sandbox restored from the same image starts its own chain from it.
Every successful save with a pages file, full or incremental, becomes the
parent of the next incremental save; one without (a compressed checkpoint)
leaves none.

### Fold-back on failure

If a save fails after its epoch ended (a pages file on a full disk, a
verification that found an escape, a failed parent check), `endDirtySave`
returns the epoch's dirty sets to the MemoryFiles (`UnswapDirty`), and the
parent stays what it was. The next incremental save, of the same parent,
therefore writes both the pages written before the failed save and those
written since: Firecracker's rule for diff snapshots.

### Saving during a background restore

A save after `runsc restore --background` would wait for every page to be
loaded (`MemoryFile.AwaitLoadAll`). A delta does not need to: a page still
being loaded has not been written since the restore (writing it waits for its
load), so it is clean and refers to the parent's extent without being read.
`SaveTo` with `Clean` therefore neither waits for the loader nor reads clean
pages, and its zero scan treats a clean page as zero exactly when it is not
committed, since a page still loading reads as zeroes. An incremental
checkpoint taken right after a background restore costs what its dirty pages
cost.

### Private MemoryFiles

The MemoryFiles of tmpfs and overlay filestores outside the application
MemoryFile (`vfs.PrepareSave`) are tracked and saved like it, each as a delta
of the parent's MemoryFile of the same owner. A MemoryFile that is not in the
parent, or was not tracked since, is saved whole in the same image.

### Restoring a chain

Restoring an incremental image needs its layers, found by `--layer-path` or in
the image's `layers/` directory; each is checked against the digest the image
records and opened once, and every range of memory is loaded from the layer
that holds it (see the image format). Depth costs open files, not reads.

### Containerd

The shim maps containerd's parent checkpoint onto runsc's flags:
`CreateTaskRequest.parent_checkpoint` becomes `runsc restore --layer-path`, and
gVisor's `CheckpointRequest.parent_image_path` becomes
`runsc checkpoint --parent-image-path` (containerd's own
`CheckpointTaskRequest` has no parent). A parent without a checkpoint is
refused.

## Pre-copy

Even a delta pauses the sandbox for the pages it writes, and a full checkpoint
for all of memory: 2.6 s for 256 MiB on a store that takes 100 MiB/s, against
a floor of 25 to 160 ms to quiesce the sandbox and save its object graph.
`runsc checkpoint --precopy=on` writes memory while the sandbox runs, as VM
live migration does, so that the pause writes only what the sandbox wrote
during the last of several rounds. It applies to full and incremental
checkpoints alike, and its images are ordinary images.

### Rounds

`Kernel.Precopy` (`pkg/sentry/kernel/precopy.go`), called by
`state.SaveOpts.Save` before it pauses the sandbox, starts the save's pages
file writer (`pgalloc.AsyncPagesFileSave`) and runs rounds. Each ends the dirty
tracking epoch with tasks running (`DirtyEpoch`'s non-paused form) and copies
pages to the pages file:

-   round 0 copies every page that may hold data (known-committed ranges, and
    the `SEEK_DATA` ranges of the others, so that holes are not read), or, for
    a delta, the pages dirtied since the parent (all of a MemoryFile that the
    delta saves whole);
-   round *k* copies the pages dirtied during round *k*−1.

The rounds copy the application MemoryFile and the private MemoryFiles that
the save saves (those of tmpfs filesystems and overlay upper layers backed by
a filestore, which `vfs.PrepareSave` records; the Kernel finds them, while
tasks run, as `tmpfs.MemoryFileOf` the Kernel's filesystems). Dirty tracking
starts at the end of a save or restore, for the MemoryFiles it saved or
loaded: a pre-copy starts it, with the Kernel paused for the arming, for those
that are not tracked yet, the application MemoryFile at a Kernel's first save
among them. Rounds track first writes in single pages (Sentry write-protection
otherwise tracks 64 KiB units, in which random writes dirty every unit of a
large image within a round, so that rounds never converge).

The save then runs in its usual pause. Its epoch, which `endDirtySave` verifies
and folds back on failure, is the union of the rounds' and its own: the pages
dirtied since the pre-copy started. The save writes the pages dirtied during
the last round; every other page refers to its last copy, wherever it is in the
pages file (`pgalloc.SaveOpts.Precopy`). Copies that a later round or the save
superseded stay in the pages file unreferenced, which keeps it a stream that an
object store accepts, at the cost of its size (1.0 to 2.1 times the image
below). A delta's MemoryFiles that the pre-copy did not copy are saved as
deltas of the pages dirtied since the parent, rounds included.

### The copier

`SaveTo` assumes a stopped sandbox; the copier (`pgalloc.Precopy`) reads
MemoryFiles in use. It takes no reference on the pages it copies, unlike
async page loading, which writes into them: its correctness comes from dirty
tracking. Every change to a page's contents dirties it, and each round re-arms
tracking before copying, so a copy that raced with a write, a free or a
reallocation is superseded by the next round or the save, and a page that the
save finds clean holds the contents of its last copy, whose hash (for the
image's page hashes) is computed after the copy was written. A page dirtied
while it was not allocated is not copied and is marked as of unknown contents,
so that the save reads it whatever its dirty state: a reallocated page can
become known-committed without being written (a read, then `UpdateUsage`), and
would otherwise refer to its previous contents. Dirty tracking verification
hashes possibly-committed pages too, so that tracking can start while tasks
run, before a save has made every page known-committed.

### Stop rule

After each round, the bytes dirtied since it started are *pending*, and its
cost per MiB is measured (the last measure is kept when a round writes less
than 1 MiB). The rounds stop:

-   when the pending bytes would take at most the budget (`--precopy-budget`,
    100 ms) to write: QEMU's rule (`migration.c`), which Cloud Hypervisor uses
    too;
-   after `--precopy-max-rounds` (8), QEMU's cap;
-   when a round does not halve the bytes left to write: the sandbox then
    dirties memory at least half as fast as the store takes it, and more
    rounds would mostly rewrite the same pages (experiment 06's prototype,
    without this rule, wrote 7.3 times the image in 8 rounds at 200 MiB/s
    against a 100 MiB/s store, for a pause no shorter).

Whatever stops them, the save follows (Cloud Hypervisor's "ignore"
timeout); a pre-copy never aborts a checkpoint.

The kvm platform maps memory into its guest without marking it dirty
(`MapInternalUntracked`), so that rounds converge there as on systrap: with
tracked `MapInternal`, every unit mapped writable (as much as a pma at a
time) was marked, the marks made while tasks run were carried into the next
epoch, and the halving rule stopped the rounds after the first. A model of
random writes at rate *r* over *N* MiB, *N*(1 − e^(−*rt*/*N*)) MiB dirty
after *t*
(experiment 06's `sim06.py`), predicts the rounds within one, and
`TestPrecopyStopRule` drives the rule with it.

### `--precopy=auto`

`auto` skips the rounds when the pages the save would write, all of memory or
a delta's dirty pages, would take at most the budget at the cost of writing
pages that the Kernel last measured, by a save or a round that wrote at least
1 MiB (`/checkpoint/pages_write_cost`): on a local disk with `O_DIRECT`, about
240 MiB per 100 ms. The measure is the sandbox's, not the store's: the Sentry
does not know which store a pages file goes to, and a restored sandbox has
none until its first save.

Without a measure, the first round measures it. Deciding earlier, during round
0's first second, could not shorten the pause: if memory fits the budget at
the speed measured, round 0 writes it within the budget, before that second
ends for any budget under a second, and the stop rule then stops the rounds,
the bytes dirtied during round 0 being at most memory; for larger budgets,
stopping round 0 early would move the rest of memory into the pause. So the
first checkpoint with `auto` always runs round 0, and the later ones decide
before any round.

### Metrics

Metric                                | Kind                                       | What
------------------------------------- | ------------------------------------------ | ----
`/checkpoint/precopy_rounds`          | counter                                    | rounds run
`/checkpoint/precopy_bytes`           | counter                                    | bytes the rounds wrote
`/checkpoint/precopy_round_bytes`     | distribution                               | bytes each round wrote
`/checkpoint/precopy_pending_bytes`   | distribution                               | bytes left after each round
`/checkpoint/pages_write_cost`        | gauge, ns per MiB                          | the last cost measured by a save or round
`/checkpoint/precopy_stops`           | counter, field `reason`                    | pre-copies by why their rounds stopped: `converged`, `round_cap`, `not_halved`, `skipped`
`/checkpoint/precopy_pause`           | distribution, ns                           | the pause of each save that completed a pre-copy
`/checkpoint/precopy_longest_stall`   | distribution, ns                           | per pre-copy, the longest that tasks were kept from running before the pause

Tasks are kept from running while a pre-copy starts dirty tracking (a Kernel
pause) and while it re-arms the dirty sources for a round (Sentry
write-protection pauses the Kernel to arm every MemoryManager).

### Measurements

Experiment 06's prototype (256 MiB rewritten at random pages, systrap, 4
shared vCPUs; a store limited to 100 MiB/s by cgroup `io.max`; budget 100 ms;
63 restores passed a page-by-page check):

Store                   | Dirty rate         | Stop the world | Pre-copy
----------------------- | ------------------ | -------------- | --------
local disk, `O_DIRECT`  | 1 to 200 MiB/s     | 136–189 ms     | 29–69 ms, 1 round, 1.00–1.04 times the image written
100 MiB/s               | 1 / 10 / 50 MiB/s  | ~2,630 ms      | 116 / 39 / 136 ms, 1 / 2 / 4 rounds, 1.0 / 1.1 / 1.7 times
100 MiB/s               | 200 MiB/s          | 2,647 ms       | 2,284 ms: 8 rounds without converging, 7.3 times

This implementation, on the same workload (`checkpoint --leave-running
--direct`, median of 3, pause from the Sentry's log; 48 restores, no bad
page; verified pre-copies at 50 and 200 MiB/s found no escaped write):

-   local disk: stop-the-world 184, 203, 188 and 192 ms at 1, 10, 50 and
    200 MiB/s; pre-copy 37, 30, 51 and 68 ms, in 1 round, 1.00 to 1.04 times
    the image written;
-   writes limited to 100 MiB/s: stop-the-world 2,626 to 2,643 ms; pre-copy
    62, 56 and 118 ms at 1, 10 and 50 MiB/s, in 1, 2 and 4 rounds, 1.0, 1.1
    and 1.7 times the image; at 200 MiB/s the halving rule stops the rounds
    after the first, for a 2,339 ms pause and 1.85 times the image (the
    prototype: 8 rounds, 7.3 times).

The fork's benchmark (tier 3, run 37993424590: a C workload of 512 MiB that
rewrites 5 or 12 % of it per second, Sentry write-protection, `--direct`,
writes limited to 100 MiB/s, 3 runs): at 5 %/s, pre-copy pauses 83 ms
(82–89) against 5.40 s stopping the world, after 4 rounds that wrote 694
MiB; at 12 %/s, more than half the store's speed, the halving rule stops the
rounds after the first (512 MiB), and the pause is 3.42 s against 5.42 s.

## Templates

Nothing in an image knows its children, so any image can be the parent of
many deltas: every sandbox restored from a template starts its own chain from
it, and their checkpoints share the template's pages, which are stored once.
Most of a template's memory stays unchanged: 83 % of a Python REPL with numpy
and pandas imported is identical, at the same offsets, after a session that
built a DataFrame, ran pytest and edited code, and 87 to 95 % of npm and Vite
workspaces after their install or start. `runsc image rebase --onto` makes an
image saved otherwise refer to a template's pages where they are the same.

## Compaction

A chain grows by its deltas: without compaction, the sessions above end with
chains of 1.14 times their image for Python, 1.37 for an idle Vite server after
5 minutes and 1.82 for Bun, whose idle collector rewrites about 13.5 MiB per
period. Since every range is read from one layer, the depth of a chain costs
open files at restore, while its size costs storage and transfers. Rewrite a
chain (`runsc image flatten`, or `runsc image compact --keep-layer` to keep
referring to a template), off the restore path, and restore from the result,
when:

-   the sum of its deltas reaches the size of its base image;
-   one delta exceeds half of the image, at which point a full checkpoint costs
    about as much;
-   or it is deeper than 16 images, which bounds the files a restore opens.

A full checkpoint (no `--parent-image-path`) starts a new chain too. The
syscall tests' `_save_incremental` variants do that every 16 saves: a test
makes thousands of saves, and unbounded chains made each restore look through
thousands of layers.

Do not checkpoint idle runtimes on a timer: Bun and Node rewrite 5 to 14 MiB
per idle period, which each incremental checkpoint then saves.

## Performance

Measured on systrap, with Sentry write-protection at its 64 KiB unit:

-   On the Python REPL above, an incremental checkpoint after a small edit
    wrote 1.96 MiB (pages changed: 1.9; tracked units without refinement:
    21.5), after a tiny exec 0.49 MiB (0.5; 4.5), and took 0.08 to 0.11 s.
-   A workload that writes memory through every path a write can take
    (`test/cmd/dirty_tracking/escapes`), checkpointed incrementally after each
    of its 14 steps, each checkpoint of the previous one, with dirty tracking
    verified: the deltas held what each step wrote (12.0 MiB of file reads,
    8.0 of loopback TCP, 2.1 of pipes, 10.5 of tmpfs writes, 12.0 of fork and
    copy-on-write, 0.02 of an exec, 256 of a 256 MiB allocation), and
    incremental checkpoints of small deltas took 28 to 62 ms. Chains of 15
    images restored the workload's memory unchanged.
-   A workload dirtying memory for 8 s wrote 205 MiB: the incremental
    checkpoint saved 210 MiB (1.019 times what was written) in 0.077 s, with a
    0.071 s pause, against 0.153 s and a 0.148 s pause for a full checkpoint.

## Tests

-   `pkg/sentry/pgalloc`: a delta saved while pages are still loading.
-   `pkg/sentry/kernel`: deltas of deltas refer each page to the layer that
    holds it, and drop layers that hold none; the parent check; a failed save
    keeps the parent; Firecracker's test that a diff snapshot failing with
    `ENOSPC` partway loses no dirty page, restoring the chain afterwards.
-   `runsc/container`: `TestCheckpointIncrementalNoEscapes` is CRIU's zdtm
    "pre-dump and compare" and Firecracker's comparison of full and diff
    snapshots, on gVisor's images: after each step of `escapes`, an
    incremental and a full checkpoint of the same moment, compared page by page
    (ignoring the VDSO parameter page, which the Sentry updates by itself), and
    the chain restored into the container that runs the next step. Its negative
    controls disable one marking path at a time
    (`--TESTONLY-dirty-tracking-break`), and must fail, by verification or by
    the comparison. `TestCheckpointIncrementalMemTouch` is CRIU's `mem-touch`:
    random writes checked against a shadow table across a chain of
    incremental checkpoints and restores.
-   The syscall tests' `_save_incremental` variants save every test
    incrementally, verified, and restore from the chain.
-   Pre-copy: in `pkg/sentry/pgalloc`, copies racing with writes and
    reallocations; in `pkg/sentry/kernel`, the stop rule driven by
    experiment 06's model, saves completing pre-copies (full, delta, private
    MemoryFiles, a delta's MemoryFile left uncopied), a pre-copy failing in
    round 0 or 1 that loses no dirty page, and `--precopy=auto`, with a store
    slowed by `stateio.RateLimitedWriter`.

## Prior art

-   CRIU's pre-dumps and `--prev-images-dir`: each dump's pagemap marks pages
    as "in parent" (`PE_PARENT`), which a restore resolves image by image;
    here, chains are resolved when an image is saved.
-   Firecracker's diff snapshots, its rule that a failed snapshot folds its
    dirty bitmap back, and its test of it.
-   containerd's `parent_checkpoint`, which the shim maps.
-   QEMU's pre-copy migration: its stop rule (pending bytes at the measured
    bandwidth within the downtime budget) and round cap; Cloud Hypervisor's
    same rule and its timeout that completes rather than aborts.
-   CRIU's pre-dump iterations, each a delta of the previous, which pre-copy
    folds into one image.
