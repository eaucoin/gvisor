# Checkpoint Images in S3-Compatible Object Stores

Status as of 2026-10-09: Implemented (`runsc/checkpointgofer/s3`).

## Synopsis

Let `runsc checkpoint` and `runsc restore`, including `runsc restore
--background`, keep checkpoint images in any S3-compatible object store (Amazon
S3, MinIO, SeaweedFS, Ceph's RADOS Gateway and others), as they already can in
Google Cloud Storage (GCS).

Non-goals:

-   A filesystem view of the store: a mounted object store (e.g. JuiceFS) already
    works as a local image path.
-   Moving images between stores, or managing their lifetime: images are
    objects, which the store's own tools copy, list and expire.

## Background

### The checkpoint gofer

A checkpoint image is a directory of files: `checkpoint.img` (the kernel's
state), `pages_meta.img` and `pages.img` (the application's memory), and, for
an image with layers, `layers/<digest>/pages_meta.img` and
`layers/<digest>/pages.img`. The Sentry reads and writes them through
`stateio.AsyncReader` and `stateio.AsyncWriter`, which allow many reads or
writes in flight.

When the image path holds `gcs_opts.json`, runsc does not open these files:
it starts the *checkpoint gofer*, a separate binary
(`runsc-checkpointgofer`), and gives the Sentry one end of a socket pair
connected to it. The gofer serves the files with `stateipc`: the Sentry opens a
file by name, and its reads and writes travel over a flipcall channel and
shared memory to the gofer, which implements
`stateipc.AsyncFileServerImpl{Destroy, OpenRead, OpenWrite}` by returning an
`AsyncReader` or `AsyncWriter` of its own for each file. The gofer is a
separate binary to keep `net/http`, and with it a storage SDK, out of runsc:
in runsc, `net/http` makes netpoll fail in the filesystem gofers, which cannot
find `/etc/hosts` (`runsc/checkpointgofer/README.md`).

Each gofer process is started for one operation and allowed only the files
of that operation: `-allow-checkpoint-reads` for a restore,
`-allow-checkpoint-writes` for a checkpoint, and their filesystem checkpoint
counterparts.

### What a remote image needs from its store

A restore with `--background` loads `pages.img` while the application runs,
and a page fault waits for the read of its page. Object stores serve ranged
GETs with a latency of milliseconds and a high bandwidth only with large reads
in parallel: a SeaweedFS store on the same host delivered 920-960 MiB/s with
4-16 MiB reads, 4-16 in flight, and half as much with 1 MiB reads (a quarter
with 256 KiB). The loader's budget of background reads (`pkg/sentry/pgalloc`)
measures the store's bandwidth and latency and keeps page faults ahead of
background reads; the reader must let a fault's read be issued at once
(`stateio.WaitOrAsyncReader`, forwarded over `stateipc` as
`AsyncFileServer.Wake`).

## Design

### Selection and options

The image path holds `s3_opts.json` instead of `gcs_opts.json`; both is an
error. runsc keeps a table of stores (`checkpointGoferStores` in
`runsc/sandbox/sandbox.go`: options file name, the gofer's flag, the URI scheme
for logs), opens the options file itself, and passes it to the gofer as a file
descriptor (`-s3-opts-fd`), as it passes `gcs_opts.json` (`-gcs-opts-fd`).

The options are JSON, decoded strictly, so that a misspelled option is an
error rather than ignored:

```json
{
  "endpoint": "https://s3.example.com",
  "region": "us-east-1",
  "bucket": "checkpoints",
  "object_prefix": "my-sandbox/",
  "addressing": "path",
  "credentials": {"access_key_id": "...", "secret_access_key": "...", "session_token": "..."},
  "credentials_file": "/etc/runsc/s3-credentials",
  "profile": "checkpoints",
  "max_attempts": 5,
  "request_timeout_seconds": 30,
  "read_bytes": 16777216,
  "read_parallel": 8,
  "write_part_bytes": 33554432,
  "write_parallel": 4
}
```

Only `bucket` is required. `credentials` excludes `credentials_file` and
`profile`. The [user guide](../user_guide/checkpoint_restore.md) documents each
option.

### The S3 backend

`runsc/checkpointgofer/s3` is the GCS backend's sibling: `s3.FileServer`
implements `stateipc.AsyncFileServerImpl`, with the same per-operation
allowances, and names the object of each file `object_prefix + name`.

**Reads** are ranged GETs, one goroutine per slot. The pages file is read in
16 MiB reads, 8 in flight, by default (GCS's read size, and the best first
answer and load time from the store above); other files in 1 MiB reads. A
response whose body ends early is resumed with a GET of the rest. The reader
implements `WaitOrAsyncReader`, so that a page fault's read is issued as soon
as the Sentry asks for it rather than at the next completion.

**Writes** are gathered into parts and uploaded as a multipart upload: 32 MiB
parts, 4 uploaded at once, for the pages file (GCS's write size); a file smaller
than a part is uploaded with one PUT. A write completes once its bytes are in a
part, so the writer holds at most `write_parallel + 1` parts in memory.
`Finalize` completes the upload; `Close` aborts an upload that `Finalize` did
not complete, so that a failed checkpoint leaves no parts behind. A gofer that
is killed cannot abort: stores should delete incomplete uploads after a day (a
bucket lifecycle rule).

**Retries and deadlines.** Requests are retried on throttling, server errors
and connection errors, with exponential backoff, up to `max_attempts` (5).
The AWS SDK's client-wide retry quota is disabled: after a burst of errors it
would fail every later request of a restore, however briefly the store failed.
Each request, with its retries, has a deadline of `request_timeout_seconds`
(30) plus the time its bytes take at 1 MiB/s, so that a stalled store fails a
checkpoint or restore instead of hanging it. A read that finally fails returns
an error, which the loader makes sticky and runsc turns into the failure of
the restore (the sandbox is killed rather than left running with missing
memory). The SDK's checksums of whole objects are off: they cannot check ranged
reads, not every store accepts them on uploads, and checkpoint images carry
their own page hashes.

**Layers** are objects too: the gofer serves
`layers/<digest>/pages_meta.img` and `layers/<digest>/pages.img` for reading
(`checkpointimage.ParseLayerPath`), so an image with layers restores from the
store when its parents' layers are under the same prefix. The same change lets
the GCS backend read layers.

The Sentry needs nothing new: the store is reached through `stateipc` as GCS
is.

### The AWS SDK for Go v2

The backend uses the AWS SDK for Go v2 (`service/s3`, `config`,
`credentials`) rather than a hand-written client. The SDK signs requests
(Signature Version 4) and resolves credentials from every source deployments
use: static keys, shared credentials and configuration files with profiles,
the environment, web identity tokens (Kubernetes service accounts), container
credentials and instance metadata. A hand-written signer would be a few hundred
lines, but each credential source is more, and each would have to be kept up to
date with AWS's. The SDK's modules are linked into the checkpoint gofer only,
not into runsc.

## Security

### How credentials reach the gofer

The sandbox never sees credentials: the Sentry talks only to the gofer, over
`stateipc`, and the gofer opens only the files of the image its flags allow
(names checked against the image's file names; layer digests parsed).

Credentials reach the gofer in one of three ways:

1.  In `s3_opts.json`, which runsc opens, as its own user, and passes to the
    gofer as a file descriptor; no path or content is passed on a command
    line or in the environment.
2.  In the file that `credentials_file` names (with `profile`), which the
    gofer's AWS SDK reads.
3.  Through the SDK's default chain: the environment that runsc was started
    with (the gofer inherits it), files under the gofer's `HOME`, a web
    identity token file, or the container or instance metadata services that
    the host's network reaches. These are the host's credentials, as for any
    other AWS client on the host.

### File ownership and permissions

The gofer refuses (`s3.CheckFileMode`) an options file or `credentials_file`:

-   owned by a user other than root and the gofer's own (runsc's) user;
-   that users other than its owner may write: such a user could point
    checkpoints, which hold the sandbox's memory, to a store of their own, or
    restore the sandbox from an image of their own;
-   that holds credentials (an options file with `credentials`, and any
    `credentials_file`) and that users other than its owner may read.

It refuses rather than warns, as ssh does with private keys: a warning lands in
a log that no one reads while the credentials stay exposed, and the error says
how to fix the file (`chmod 600`). The SDK's default files (under `HOME`) are
the host's own configuration and are not checked, as other AWS clients do not
check them. The GCS gofer's `gcs_opts.json` keeps upstream's behavior.

### Credentials are never logged

Options are decoded from the file descriptor and never printed: runsc logs the
store's name and the image's URI (`s3://bucket/prefix`), the gofer logs object
names, and errors about options print the values at fault only (the options
struct holds the credentials provider, so it is never formatted whole;
`TestNewFileServerErrorsHoldNoCredentials`). The SDK's request logging is off.

### The gofer process

runsc starts the gofer with `setsid`, in the sandbox's cgroup, as runsc's own
user, with runsc's environment and standard streams on `/dev/null`. Like the
GCS gofer it runs on the host, outside the sandbox: it needs the host's network
to reach the store, and it has no seccomp filter and keeps runsc's
capabilities. Its exposure is what it parses: the store's responses (through the
SDK's HTTP client) and the Sentry's `stateipc` requests, whose file names it
checks against the image's. A compromised Sentry can therefore read and write
only the objects of the image being restored or checkpointed, which it holds
anyway.

Confining the gofer further (dropping capabilities it does not need, a seccomp
filter fitted to Go's HTTP client, a user of its own) would apply to the GCS
gofer as well, and is left as future work.

## Performance

Measured with a SeaweedFS 4.47 store on the same host as the sandbox (4 shared
CPUs), signed requests, images out of the store's page cache, median of 4:

| Workload                                | Checkpoint | `runsc restore --background` returns | First answer | All pages loaded |
| --------------------------------------- | ---------- | ------------------------------------ | ------------ | ---------------- |
| Python faulter, 393 MiB image, S3       | 2.4 s      | 0.25 s                               | 0.58 s       | 0.83 s           |
| Python faulter, 393 MiB image, local    | 0.45 s     | 0.17 s                               | 0.23 s       | 0.22 s           |
| Python REPL (pandas), 89 MiB image, S3  |            |                                      | 0.27 s       | 0.16 s           |
| Python REPL, 89 MiB image, local        |            |                                      | 0.17 s       | 0.07 s           |

-   The REPL restores from the store as from local disk but for the start of
    the gofer and its reading `checkpoint.img`, about 0.1 s.
-   For the faulter, the loader's slow start ended at one 16 MiB read in
    flight: the store delivered 340-680 MB/s in 22-34 ms per read, which the
    store's own CPU bounds. Probing for more reads in flight (BBR's ProbeBW, in
    `pkg/sentry/pgalloc`) then loads all pages in 0.56-0.94 s, depending on the
    store's load. Eight parallel 16 MiB GETs by curl deliver 738 MiB/s from the
    same store, a lower bound of 0.53 s for this image.
-   With the image's working set read first (`--working-set-prefetch`), or
    laid at the head of `pages.img` (`runsc image compact
    --working-set-first`), the faulter answers after 0.50 s or 0.40 s instead
    of 0.88 s (median of 3, in a later run).
-   First touches of the faulter's data wait 0.2 ms (median) while pages load.

## Testing

-   `runsc/checkpointgofer/s3/s3test` is an in-memory S3 store with fault
    injection. The backend's unit tests read ranges, retry server errors and
    connection resets, resume short bodies, fail at a deadline on a stalled
    store, wake a waiting reader, write single objects and multipart uploads,
    abort failed and unfinished uploads, refuse files outside an operation's
    allowance, and check file permissions and that errors hold no credentials.
-   `TestCheckpointRestoreS3` (`runsc/container`) checkpoints a container
    through the gofer to `s3test` and restores it with `--background`.

## Open questions

-   The measurements are of a store on the same host. A store across a network
    adds its round trip to every request, which the 16 MiB reads and the
    loader's latency-aware budget are designed to absorb, but no one measured.
-   Confining the checkpoint gofer (see above).
