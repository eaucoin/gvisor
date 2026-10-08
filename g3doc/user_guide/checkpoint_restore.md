# Checkpoint/Restore

[TOC]

gVisor has the ability to checkpoint a process, save its current state in a
state file, and restore into a new container using the state file.

## How to use checkpoint/restore

Checkpoint/restore functionality is currently available via raw `runsc`
commands. To use the checkpoint command, first run a container.

```bash
runsc run <container id>
```

To checkpoint the container, the `--image-path` flag must be provided. This is
the directory path within which the checkpoint related files will be created.
All necessary directories will be created if they do not yet exist.

> Note: Two checkpoints cannot be saved to the same directory; every image-path
> provided must be unique.

```bash
runsc checkpoint --image-path=<path> <container id>
```

There is also an optional `--leave-running` flag that allows the container to
continue to run after the checkpoint has been made. (By default, containers stop
their processes after committing a checkpoint.)

> Note: All top-level runsc flags needed when calling run must be provided to
> checkpoint if `--leave-running` is used.

> Note: `--leave-running` functions by causing an immediate restore so the
> container, although will maintain its given container id, may have a different
> process id.

```bash
runsc checkpoint --image-path=<path> --leave-running <container id>
```

To restore, provide the image path to the directory containing all the files
created during the checkpoint. Because containers stop by default after
checkpointing, restore needs to happen in a new container (restore is a command
which parallels start).

```bash
runsc create <container id>

runsc restore --image-path=<path> <container id>
```

> Note: All top-level runsc flags needed when calling run must be provided to
> `restore`.

## Optimizations

gVisor supports several performance optimizations during checkpoint and restore.
These can be configured via flags provided to the `runsc checkpoint` and `runsc
restore` commands.

### Compression

By providing the `--compression` flag to `runsc checkpoint`, users can specify
the compression level of the generated snapshot files. Supported values are
`none` (default) and `flate-best-speed`.

Note that `--compression=none` consumes less CPU and is faster. The generated
snapshot contains multiple files. As a result, it allows the kernel and memory
restores to proceed in parallel. Furthermore, several other optimizations
described below require `--compression=none`.

### Exclude Committed Zero Pages

By providing the `--exclude-committed-zero-pages` flag to `runsc checkpoint`,
gVisor skips saving memory pages that are committed but contain only zeros. This
can significantly reduce the checkpoint size for applications that have large,
zero-filled memory regions (like LLMs), thereby speeding up restore. However, it
may increase checkpoint duration, as it requires scanning all committed pages to
determine if they are zero-filled.

### Direct I/O

By providing the `--direct` flag to `runsc checkpoint` or `runsc restore`,
gVisor uses `O_DIRECT` when writing or reading the pages file. This bypasses the
host page cache. This optimization requires `--compression=none` during
checkpoint. This is only supported on filesystems that support direct I/O.

This is particularly advantageous when the snapshot is being read for the first
time from disk and will not be restored on the same machine again, making
caching in the host page cache undesirable.

### Background Restore

By providing the `--background` flag to `runsc restore`, the application can
start execution as soon as the kernel state is loaded. The remaining application
memory and file data are restored asynchronously in the background while the
application is running. This optimization requires `--compression=none` during
checkpoint.

If the application accesses a memory page that has not yet been restored, gVisor
prioritizes loading that page immediately to unblock the application thread.
This can dramatically reduce the "Time to First Instruction" for large
applications.

A page the application touches waits for its own read, not for the background
reads already under way. gVisor maps memory that is still loading 64 KiB at a
time, and it bounds the background reads it keeps in flight to what keeps the
storage busy: the bandwidth the storage delivers times the latency of a
background read, plus 2 ms. On a disk, which serves reads in order, a touched
page then waits for about one background read and 2 ms; on a store that serves
reads in parallel, such as an object store behind the checkpoint gofer, it does
not wait for them at all. Loading starts with four reads in flight and grows
until reads begin to queue, as TCP's slow start does. The log line "Async page
loading completed" reports how many times and how long the application waited
for pages, as do the metrics `/checkpoint/async_load_waits`,
`/checkpoint/async_load_wait_bytes` and `/checkpoint/async_load_wait_nanoseconds`
(see [observability](observability.md)).

Note that when this is enabled, the sandbox may continue to have an open FD on
the snapshot files even after the sandboxed application has started. This means
that until the sandbox has fully restored (async page loading has completed):

-   Deleting the pages file may not free disk space immediately on POSIX
    filesystems.
-   Deleting the pages file may not be possible on non-POSIX filesystems.
-   The mount containing the snapshot files cannot be unmounted.

You can use `runsc wait --restore` to wait for restore to complete fully, after
which you can clean up the `--image-path` directory if necessary.

## Checkpoint images and their layers

An uncompressed checkpoint image is a directory of three files:

-   `checkpoint.img`: the state of the sandbox's kernel, and metadata such as
    the version of runsc that saved it. A runsc binary restores only images
    saved by the same version.
-   `pages.img`: the contents of memory pages, page-aligned, in any order.
-   `pages_meta.img`: where the contents of each saved page are, then a hash of
    each saved page. It starts with the magic `gVisorPM` and a major and minor
    format version (a build of runsc reads only its own major version, and
    minor versions up to its own), and its header and contents are protected by
    CRC-64 checksums, which `runsc restore` checks before reading anything
    else. A restore does not wait for the page hashes, which are 2 MiB per GiB
    of memory.

The SHA-256 of `pages_meta.img` up to its page hashes identifies the image: it
covers the location of every saved page and the digest of the page hashes, so
it changes whenever the image's memory does. `runsc image inspect` prints it.

The pages of an image may be held by the `pages.img` of other images, its
*layers*, which `pages_meta.img` names by their identity. However long the
chain of images that produced it, every range of memory is read directly from
the layer that holds it. To restore such an image, `runsc restore` looks for
each layer, in order:

1.  in the image's own directory, at `layers/<identity>/` (a directory holding
    the layer's `pages_meta.img` and `pages.img`, or a symbolic link to one);
2.  in each directory given with `--layer-path`, which may be the layer's image
    directory itself or a directory of image directories named by their
    identity.

`runsc restore` checks the identity of each layer it finds and the size of its
`pages.img`, and fails before starting the sandbox if a layer is missing or
does not match:

```bash
runsc restore --image-path=<path> --layer-path=<parent image path> <container id>
```

Images read through a checkpoint gofer (such as `gs://` image paths) find their
layers under the image's prefix, as objects `layers/<identity>/pages_meta.img`
and `layers/<identity>/pages.img`.

With `--background`, pages are loaded from every layer in parallel, each
layer's `pages.img` read from start to end, and an access to a page that is not
loaded yet waits for the layer that holds it.

### Inspecting and rewriting images

`runsc image` reads, checks and rewrites uncompressed checkpoint images without
a sandbox. It never modifies an image: commands that rewrite one write a new
image directory, synced before they succeed.

-   `runsc image inspect [--json] IMAGE` prints the image's identity, format
    version, state file metadata (runsc version, platform, CPU features, time),
    its layers and where they were found, the memory of each MemoryFile and
    which layers hold it, and its working set. The JSON output is meant for
    other tools, such as checkpoint managers listing the checkpoints of every
    runtime; fields are only ever added to it.
-   `runsc image verify [--pages] [--host] IMAGE` checks the checksums and
    consistency of `pages_meta.img` and the identity and size of every layer;
    with `--pages`, it reads every page and checks it against its hash; with
    `--host`, it checks that this runsc can restore the image on this host (the
    same runsc version, the platform, and every CPU feature the image was saved
    with). Its exit status is 3 if the image is invalid or corrupt, 4 if a layer
    is missing, and 5 if the image cannot be restored here.
-   `runsc image flatten --output=DIR IMAGE` writes an image with the same
    memory, all of it in its own `pages.img`: no layers.
-   `runsc image compact --output=DIR [--keep-layer=DIGEST]... IMAGE` does the
    same, but pages held by the kept layers stay there. The new `pages.img` holds
    only data that the image refers to.
-   `runsc image rebase --onto=BASE --output=DIR IMAGE` writes an image with the
    same memory whose pages refer to BASE wherever BASE has the same page at the
    same place, as is the case for much of the memory of a sandbox restored from
    BASE: images of sandboxes restored from a common template can share its
    pages.
-   `runsc image layers IMAGE` prints the identity of each image that IMAGE
    refers to, for an image store's garbage collection: an image can be deleted
    when no image kept lists it.

The commands that rewrite an image take `--working-set-first`: the pages of the
working set recorded in the image then come first in the new `pages.img`, in
the order the sandbox touched them, so that a restore, which loads `pages.img`
in order in the background, loads them first. All of them take
`--layer-path`, as `runsc restore` does.

## Incremental checkpoints

With `--dirty-tracking=wp` (a runsc flag, given when the sandbox is created or
restored), the sandbox tracks the memory pages written between checkpoints. A
checkpoint can then be incremental: given the image the container was last
checkpointed to or restored from, it writes only the pages written since, and
refers to that image, its parent, for the others:

```bash
runsc --dirty-tracking=wp run <container id>
runsc checkpoint --image-path=<image 1> --leave-running <container id>
runsc checkpoint --image-path=<image 2> --parent-image-path=<image 1> --leave-running <container id>
runsc checkpoint --image-path=<image 3> --parent-image-path=<image 2> <container id>
```

The parent and the images it refers to become layers of the new image (see
above), so restoring it needs them:

```bash
runsc --dirty-tracking=wp restore --image-path=<image 3> --layer-path=<image 1> --layer-path=<image 2> <container id>
```

A page is read from the image that holds its latest contents, and an image
refers only to the layers that hold some of its pages: a chain of incremental
checkpoints shortens itself as pages are rewritten. The parent must be the
image the container was last checkpointed to or restored from (the checkpoint
fails otherwise), and a failed checkpoint leaves it so; the first checkpoint of
a container, and the first after enabling dirty tracking, are full.

Tracking makes the first write to a page after a checkpoint fault into the
sandbox's kernel: `--dirty-tracking-unit` (64 KiB by default) is the size of
the memory that one such fault marks written, trading the cost of writes after
a checkpoint (more faults with a smaller unit) for the size of the next
incremental image (more pages with a larger one).
`--dirty-tracking-verify=hash` makes every checkpoint also check, by hashing
every page, that no page changed without being tracked, and fail if one did;
it is meant for tests and debugging.

### Tracking writes with the host kernel

With `--dirty-tracking=uffd`, the host kernel records the writes instead
(userfaultfd write-protection in asynchronous mode, read with `PAGEMAP_SCAN`;
Linux 6.7 or later), at 4 KiB. On systrap, a first write after a checkpoint
costs about 1.5 µs per page, against about 25 µs at 4 KiB and 3 µs per page
at 64 KiB with `wp`. Its other costs grow with the sandbox's memory: page
tables for the whole tracked memory (2 MiB per GiB), and reading what was
written, about 3 ms per GiB in each checkpoint. Write-protection also splits
huge pages. It is available on the KVM and systrap platforms;
`--dirty-tracking=auto` selects it on KVM, when the host supports it and
without `--app-huge-pages`, and `wp` otherwise.

On systrap, the application runs in host processes (stubs) whose page tables
the host kernel tracks for the sandbox. `uffd` widens what the sandbox's
kernel (the Sentry) may ask of the host, which is why `auto` does not select
it there. Application code cannot use either grant: it runs in stub threads
whose seccomp filter traps every system call to the Sentry.

-   The stubs may call `userfaultfd(2)` and `mprotect(2)` when the Sentry
    makes them: `userfaultfd` only with exactly the flags `O_CLOEXEC |
    O_NONBLOCK | UFFD_USER_MODE_ONLY`, so that the host kernel never stalls on
    a fault it takes itself, which is what makes userfaultfds an aid to
    kernel exploits.
-   The Sentry keeps a descriptor of the procfs it starts with (that of the
    sandbox's PID namespace, unless the sandbox runs without its own root) and
    may open files through it, read-only: it opens the stubs'
    `/proc/PID/pagemap`, but seccomp cannot check paths, so it could open any
    file of that procfs, or of its own root (an empty, read-only directory).
    `/proc` itself is not mounted in the sandbox, as without dirty tracking.
    Where that procfs is of an ancestor PID namespace, the Sentry also opens
    pidfds of its stubs, to find their PIDs there.

## Pre-copy

A checkpoint pauses the container while it writes its memory. With
`--precopy=on` (or `auto`), `runsc checkpoint` first writes memory while the
container runs, in rounds: the first writes all of memory (or, with
`--parent-image-path`, the memory written since the parent), and each next
round writes the memory written during the previous one, as VM live migration
does. Rounds stop when the memory written during the last one would take at
most `--precopy-budget` (100 ms by default) to write, at the speed measured
during that round; after `--precopy-max-rounds` rounds (8); or when a round
does not halve the memory left to write, as when the container writes memory
at least half as fast as the checkpoint can write it, where more rounds would
only write the same memory again. Then the container is paused, and the
checkpoint writes only the memory written since the last round, besides the
rest of its state. The image refers to the latest copy of every page, and is
restored as any other.

```bash
runsc --dirty-tracking=wp run <container id>
runsc checkpoint --image-path=<path> --precopy=on --direct <container id>
```

A container that writes memory faster than half the checkpoint's write speed
keeps the rounds from converging. With `--precopy-throttle=on`, the first round
that does not halve the memory left to write does not stop the rounds: from
then on until the checkpoint completes, each process may dirty memory at most
at a quarter of the write speed measured, and the processes that write faster
are delayed after the page faults that record their first writes, as QEMU's
dirty-limit does for vCPUs. The rounds then converge, at the cost of slowing
those processes during the checkpoint. Throttling requires
`--dirty-tracking=wp`: with `uffd`, which records writes without faults that
could delay them, a checkpoint with `--precopy-throttle=on` fails.

Pre-copy requires `--dirty-tracking` and an uncompressed image. It pays when
writing memory takes longer than the budget, as with large containers or slow
stores; `--precopy=auto` skips it when the previous checkpoint's write speed
says that the pause would write memory within the budget anyway. The pause
also includes saving the rest of the container's state, which does not depend
on its memory size. `--direct` is recommended: writes with `O_DIRECT` cost a
third of buffered ones per MiB.

## How to use checkpoint/restore in Docker:

Run a container:

```bash
docker run [options] --runtime=runsc --name=<container-name> <image>
```

Checkpoint the container:

```bash
docker checkpoint create <container-name> <checkpoint-name>
```

Restore into the same container:

```bash
docker start --checkpoint <checkpoint-name> <container-name>
```

### Issues Preventing Compatibility with Docker

-   **[Moby #37360][leave-running]:** Docker version 18.03.0-ce and earlier
    hangs when checkpointing and does not create the checkpoint. To successfully
    use this feature, install a custom version of docker-ce from the moby
    repository. This issue is caused by an improper implementation of the
    `--leave-running` flag. This issue is fixed in newer releases.
-   **Docker does not support restoration into new containers:** Docker
    currently expects the container which created the checkpoint to be the same
    container used to restore. This is needed to support container migration.
-   **[Moby #37344][checkpoint-dir]:** Docker does not currently support the
    `--checkpoint-dir` flag but this will be required when restoring from a
    checkpoint made in another container.

## How to use checkpoint/restore with containerd

The gVisor containerd shim, `containerd-shim-runsc-v1`, checkpoints a sandbox
through containerd's task service:

```bash
ctr task checkpoint --image-path=<path> [--exit] <container id>
```

The checkpoint holds the whole sandbox, whichever of its containers it is asked
for. With `--exit` the sandbox stops after the checkpoint; otherwise it keeps
running.

A task that containerd creates from a checkpoint (`CreateTaskRequest.checkpoint`,
which containerd sets from a checkpoint directory or a checkpoint in its content
store) is restored, rather than started, when it is started. Create and start the
sandbox's root container first: its restore brings back the whole sandbox, and
those of the other containers reattach them. Two options of the runtime's shim
configuration file (its `ConfigPath`, see
[Containerd Advanced Configuration](containerd/configuration.md)) apply to these
restores:

```toml
restore_direct = true      # runsc restore --direct
restore_background = true  # runsc restore --background
```

## Restore validation

A restore checks that the sandbox it brings back still matches what it finds on
the host, and fails rather than letting the application run on top of
something else.

### Specs

The `--restore-spec-validation` flag (`enforce` by default, or `warning` or
`ignore`) compares each restored container's OCI spec with the spec it was
checkpointed with. What places a container on the host may change: its
environment, hostname, cgroup (including the pod cgroup that the containerd
shim records in `dev.gvisor.spec.cgroup-parent`), resources and OOM score
adjustment, and the sources of its mounts. What the checkpointed processes
depend on may not: among others their arguments, user, capabilities, mounts'
destinations and options, and the image the container was created from, as its
container manager names it (`io.kubernetes.cri.image-name` for containerd,
`io.kubernetes.cri-o.ImageName` for CRI-O). So a sandbox may be restored into a
new Kubernetes pod whose name, UID, IP address, labels, memory request and
limit differ, but not with another image or another command.

### Files

A checkpoint does not contain the files of the filesystems that gVisor reaches
through its gofer, such as the containers' root filesystems: the restored sandbox
reopens them. The `--restore-validate-files` flag selects the files whose size
and modification time a restore compares with the checkpoint's, failing if
either changed:

-   `rootfs` (default): the files of the containers' root filesystems, that is,
    their images.
-   `all`: also the files of the containers' mounts.
-   `none`: no file.

Only the files that the checkpoint holds, such as running executables and their
libraries and open files, are checked: the checkpoint knows no others. A
container restored onto another version of its image, whose executable or
libraries differ, fails to restore instead of crashing. Mounts are not checked
by default because their contents may legitimately change between checkpoint
and restore, as Kubernetes rewrites `/etc/hosts` for each pod.

## Networking

Checkpoint/restore is supported with `--network=sandbox` (default),
`--network=none`, and `--network=host`.

With `--network=host`, host sockets cannot be saved, so:

-   Checkpoint with `--leave-running` does not touch the running sandbox's
    sockets. It keeps using them as before.
-   TCP listening sockets are re-created during restore and keep accepting new
    connections. Connections that were pending in the backlog at checkpoint time
    are lost. If the listen address cannot be bound on the restoring host, the
    listener socket will fail to restore (subsequent operations on it will
    return errors), but the sandbox restore operation will still succeed.
-   Sockets that were connected at checkpoint time return `ECONNRESET`, and
    `epoll_wait` on them returns `EPOLLERR | EPOLLHUP` immediately. Applications
    must reconnect after restore.
-   Network configuration visible inside the sandbox (interface statistics, TCP
    buffer sizes) reflects the host the sandbox was restored on.

## Checkpoint & Restore with different CPU features

When restoring a state file, gVisor verifies that the target host machine
possesses all the CPU features enabled on the machine where the checkpoint
snapshot was created.

gVisor allows users to specify a list of *allowed* CPU features using the
annotation `dev.gvisor.internal.cpufeatures`. Only the host CPU features present
in this annotation list will be enabled. By doing this, users are able to
stabilize the list of CPU features that will be exposed to applications in the
sandbox, which makes it possible to checkpoint and restore among machines with
different set of CPU features.

CPU features in the annotation should be comma-separated. A comprehensive list
of all supported CPU features can be found
[here](https://github.com/google/gvisor/blob/61f4c77225e1f5128cad8982f3af0d4278494bd4/pkg/cpuid/features_amd64.go#L457).

The runsc command `runsc cpu-features` lists all CPU features on the current
machine.

## GPU Checkpoint/Restore

gVisor supports checkpointing and restoring containers that use GPUs by
leveraging [cuda-checkpoint](https://github.com/NVIDIA/cuda-checkpoint).

When a snapshot is created via `runsc checkpoint`, the user can provide the
`--cuda-checkpoint-path` flag to indicate the path to the `cuda-checkpoint`
binary in the container filesystem. This enables `cuda-checkpoint` automation.

Before pausing the container, gVisor will collect all the CUDA processes in the
sandbox and checkpoint them using `cuda-checkpoint`. Note that `cuda-checkpoint`
is invoked in parallel across all processes for performance. Once all
`cuda-checkpoint` invocations succeed, the regular gVisor checkpointing
procedure continues.

On restore, after the kernel is restored and started, all CUDA processes which
were checkpointed earlier are toggled back on using `cuda-checkpoint`. `runsc
restore` does not require any special flags. If the snapshot was created with
`runsc checkpoint --cuda-checkpoint-path`, then the same configuration will
automatically be used on restore.

### Limitation

GPU checkpoint/restore is not supported on the arm64 architecture due to lack of
support in [cuda-checkpoint](https://github.com/NVIDIA/cuda-checkpoint).

[leave-running]: https://github.com/moby/moby/pull/37360
[checkpoint-dir]: https://github.com/moby/moby/issues/37344

## Application-Driven Checkpoint/Restore

In addition to the `runsc checkpoint` CLI command, gVisor lets the workload
*inside* the sandbox trigger checkpoints and synchronize with restore, without
any external call to `runsc`. This is useful for applications that want to
checkpoint at a specific, self-determined point (for example, after warming up a
cache or finishing initialization) and for applications that need to react to
being restored.

This functionality is configured entirely through OCI runtime spec
**annotations** and is exposed to the workload through files under
`/proc/gvisor/`. No new `runsc` flags are involved.

### Enabling and Configuration

The files `/proc/gvisor/checkpoint` and `/proc/gvisor/spec_environ` are always
present in the sandbox.

By default, `/proc/gvisor/checkpoint` is read-only (mode `0444`): a workload can
read it to *wait* for the next resume/restore (which might be triggered
externally, e.g., via `runsc checkpoint`).

To allow the workload to *trigger* a checkpoint from within the container:

1.  Specify where the checkpoint files should be written by setting the
    `dev.gvisor.internal.checkpoint.path` annotation on the **root/first
    container**.
2.  Enable write access to the checkpoint file by setting
    `dev.gvisor.internal.checkpoint.enable=true` on the specific container that
    needs to trigger the checkpoint. This makes `/proc/gvisor/checkpoint`
    writable (mode `0666`) for that container.

Note that `dev.gvisor.internal.checkpoint.enable` is a per-container setting,
allowing some containers to trigger checkpoints while others can only observe
them. However, the `path` annotation must be set on the root container to
configure the snapshot destination for the entire sandbox; if `path` is not set,
attempts to trigger a checkpoint will fail.

> Note: The `path` annotation must be set on the root container; it configures
> the snapshot destination for the whole sandbox. The `enable` annotation is
> evaluated per container.

### Checkpoint options

When checkpointing is driven by the workload, the options normally passed as
flags to `runsc checkpoint` are instead provided as annotations on the
root/first container:

Annotation                                                    | Description                                                                                           | Default
------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- | -------
`dev.gvisor.internal.checkpoint.path`                         | Directory where checkpoint files are written. Required to enable.                                     | (required)
`dev.gvisor.internal.checkpoint.enable`                       | Per-container; makes `/proc/gvisor/checkpoint` writable so the workload can trigger a checkpoint.     | `false`
`dev.gvisor.internal.checkpoint.resume`                       | Keep the sandbox running after the checkpoint (analogous to `--leave-running`).                       | `false`
`dev.gvisor.internal.checkpoint.compression`                  | Compression level: `none` or `flate-best-speed` (see [Compression](#compression)).                    | none
`dev.gvisor.internal.checkpoint.direct`                       | Use `O_DIRECT` for checkpoint I/O (see [Direct I/O](#direct-io)).                                     | `false`
`dev.gvisor.internal.checkpoint.exclude-committed-zero-pages` | Skip saving committed zero pages (see [Exclude Committed Zero Pages](#exclude-committed-zero-pages)). | `false`
`dev.gvisor.internal.checkpoint.cuda-checkpoint-path`         | Path to the `cuda-checkpoint` binary (see [GPU Checkpoint/Restore](#gpu-checkpointrestore)).          | (unset)
`dev.gvisor.internal.checkpoint.cuda-checkpoint-sequential`   | Run `cuda-checkpoint` sequentially instead of in parallel.                                            | `false`
`dev.gvisor.internal.checkpoint.save-restore-exec-argv`       | Argv of a binary to exec around save/restore.                                                         | (unset)
`dev.gvisor.internal.checkpoint.save-restore-exec-timeout`    | Timeout for the save/restore exec binary (e.g. `30s`).                                                | 10 minutes

### Triggering and waiting via `/proc/gvisor/checkpoint`

The `/proc/gvisor/checkpoint` file is the workload's interface to the
checkpoint/restore machinery. It behaves like a character device with the
following protocol:

1.  **Open** the file. The open registers interest in the *next* checkpoint.
    This means you can open the file and then trigger a checkpoint without
    racing against it completing.
2.  **Write** `1` (or `1\n`, so `echo 1` works) to trigger a checkpoint. Writing
    requires `dev.gvisor.internal.checkpoint.enable=true`. Writing is optional —
    a process can simply read the file to passively wait for a checkpoint
    triggered by some other process. Triggering a second save by writing to the
    same file will fail with `ENXIO`.
3.  **Read** the file. The read blocks until the checkpoint completes and then
    returns one of the following lines:
    -   `resume` — the checkpoint completed and the original workload has
        resumed running (this is what the process that triggered the checkpoint
        sees when the sandbox keeps running).
    -   `restore` — execution is continuing inside a freshly *restored*
        instance, i.e. this process is running in the restored sandbox.
    -   `error` — the checkpoint failed; the workload resumes running anyway.

After the result is determined, reads always return the same value. To wait for
a *subsequent* checkpoint, the file must be opened again.

The `resume` vs. `restore` distinction lets a workload tell whether it is the
original (post-checkpoint) process or the restored copy, which is handy for
performing different post-checkpoint vs. post-restore actions on the same code
path.

For example, from a shell inside an enabled container:

```bash
# Open FD 3, trigger a checkpoint, then read the outcome.
exec 3<>/proc/gvisor/checkpoint
echo 1 >&3
cat <&3   # blocks until resume/restore completes, prints "resume" or "restore"
```

### Reading restore-time environment via `/proc/gvisor/spec_environ`

When application-driven checkpointing is enabled, gVisor also exposes
`/proc/gvisor/spec_environ`. It contains the environment variables from the
container's spec (NULL-separated, in the same format as `/proc/<pid>/environ`).

Because the environment variables in the spec used to *restore* a container can
differ from the one used to create it, this file gives the workload a way to
read environment variables supplied at restore time. A common pattern is to wait
on `/proc/gvisor/checkpoint`, and once it reports `restore`, re-read
`/proc/gvisor/spec_environ` to pick up new configuration injected by the
restoring environment.
