![gVisor](g3doc/logo.png)

[![Build status](https://badge.buildkite.com/3b159f20b9830461a71112566c4171c0bdfd2f980a8e4c0ae6.svg?branch=master)](https://buildkite.com/gvisor/pipeline)
[![Issue reviver](https://github.com/google/gvisor/actions/workflows/issue_reviver.yml/badge.svg)](https://github.com/google/gvisor/actions/workflows/issue_reviver.yml)
[![CodeQL](https://github.com/google/gvisor/actions/workflows/codeql.yml/badge.svg)](https://github.com/google/gvisor/actions/workflows/codeql.yml)
[![gVisor chat](https://badges.gitter.im/gvisor/community.png)](https://gitter.im/gvisor/community)
[![code search](https://img.shields.io/badge/code-search-blue)](https://cs.opensource.google/gvisor/gvisor)

## This fork

This is [eaucoin/gvisor](https://github.com/eaucoin/gvisor), a fork of
[google/gvisor](https://github.com/google/gvisor) that adds checkpoint and
restore features to gVisor, as general-purpose features of `runsc`:

*   **Fault-first lazy restore**: `runsc restore --background` serves a page
    fault from the image before the background reads behind it.
*   **An S3-compatible checkpoint gofer**, beside the GCS one, to save and
    restore images straight from an object store.
*   **Restore into a new Kubernetes pod** through containerd's task service.
*   **Incremental checkpoints**: dirty-page tracking (by write-protection in the
    Sentry, or by userfaultfd), a checkpoint image format whose layers refer to
    their parent's, and saves that write only what changed since.
*   **Pre-copy**: checkpoints taken while the sandbox runs, with a short final
    pause.
*   **Working-set recording and prefetch**, and `runsc image` tools to inspect,
    verify, flatten and compact images, also usable as a checkpointctl plugin.

Each lands as a series, a branch of its own on top of the base; the
[`SERIES`](SERIES) file lists those that `main` carries, and each release's
notes list those it contains. Their documentation is gVisor's own, updated with
them: see [Checkpoint/Restore](g3doc/user_guide/checkpoint_restore.md), and for
their designs the documents in [`g3doc/proposals`](g3doc/proposals).

**How it relates to upstream.** `main`, the default branch, is an upstream
gVisor release (the base, named in `SERIES`) plus the series listed there, as
far as they pass the fork's tests, rebuilt from scratch whenever one changes; it
moves to a newer upstream release deliberately, by rebasing every series onto
it. `master` and `go` mirror upstream's, untouched. The fork's changes are not
proposed upstream; they keep gVisor's license, style and tests. Report problems
with them in this repository's issues, not upstream's.

**Using it.** Releases are on this repository's
[Releases](https://github.com/eaucoin/gvisor/releases) page, built from `main`
with upstream's release rules, with SHA-512 sums and build provenance
attestations, for x86_64 (aarch64 follows once a restore works on the aarch64
machines the fork is tested on). They are installed as upstream's are:

```sh
ARCH=$(uname -m)
URL=https://github.com/eaucoin/gvisor/releases/download/<release>
curl -fsSL -O "${URL}/gvisor-${ARCH}.tar.zstd" -O "${URL}/gvisor-${ARCH}.tar.zstd.sha512"
sha512sum -c "gvisor-${ARCH}.tar.zstd.sha512"
gh attestation verify "gvisor-${ARCH}.tar.zstd" --repo eaucoin/gvisor
sudo tar -C /usr/local/bin --zstd -xf "gvisor-${ARCH}.tar.zstd"
```

A release is tagged `release-<upstream release>-eaucoin.<n>`, which is also
what `runsc --version` reports. A checkpoint restores only under the build that
took it, never under upstream's build of the same release, so keep a release
for as long as you keep its checkpoints. To build from source, build `main` as
described below.

**Working on it.** A series is a branch `series/<NN>-<name>` holding one work
item as a clean stack of commits on the base, or on the series it builds on:
no merge commits, amended rather than fixed up, subjects in gVisor's
`area: summary` style. [`tools/fork/series.sh`](tools/fork/series.sh) checks
each series and builds `main` from them, and the fork's workflows
(`.github/workflows/fork-*.yml`) test every series alone and `main` with
gVisor's own Bazel tests on GitHub's runners, keep `master` and `go` in sync,
and cut the releases.

## What is gVisor?

**gVisor** provides a strong layer of isolation between running applications and
the host operating system. It is an application kernel that implements a
[Linux-like interface][linux]. Unlike Linux, it is written in a memory-safe
language (Go) and runs in userspace.

gVisor includes an [Open Container Initiative (OCI)][oci] runtime called `runsc`
that makes it easy to work with existing container tooling. The `runsc` runtime
integrates with Docker and Kubernetes, making it simple to run sandboxed
containers.

## What **isn't** gVisor?

*   gVisor is **not a syscall filter** (e.g. `seccomp-bpf`), nor a wrapper over
    Linux isolation primitives (e.g. `firejail`, AppArmor, etc.).
*   gVisor is also **not a VM** in the everyday sense of the term (e.g.
    VirtualBox, QEMU).

**gVisor takes a distinct third approach**, providing many security benefits of
VMs while maintaining the lower resource footprint, fast startup, and
flexibility of regular userspace applications.

## Why does gVisor exist?

Containers are not a [**sandbox**][sandbox]. While containers have
revolutionized how we develop, package, and deploy applications, using them to
run untrusted or potentially malicious code without additional isolation is not
a good idea. While using a single, shared kernel allows for efficiency and
performance gains, it also means that container escape is possible with a single
vulnerability.

gVisor is an application kernel for containers. It limits the host kernel
surface accessible to the application while still giving the application access
to all the features it expects. Unlike most kernels, gVisor does not assume or
require a fixed set of physical resources; instead, it leverages existing host
kernel functionality and runs as a normal process. In other words, gVisor
implements Linux by way of Linux.

gVisor should not be confused with technologies and tools to harden containers
against external threats, provide additional integrity checks, or limit the
scope of access for a service. One should always be careful about what data is
made available to a container.

## Documentation

User documentation and technical architecture, including quick start guides, can
be found at [gvisor.dev][gvisor-dev].

## Installing from source

gVisor builds on x86_64 and ARM64. Other architectures may become available in
the future.

For the purposes of these instructions, [bazel][bazel] and other build
dependencies are wrapped in a build container. It is possible to use
[bazel][bazel] directly, or type `make help` for standard targets.

### Requirements

Make sure the following dependencies are installed:

*   Linux 5.6+
*   [Docker version 17.09.0 or greater][docker]

### Building

Build a release tarball containing `runsc`, the `containerd-shim-runsc-v1`
containerd shim, and a few sidecar binaries that `runsc` expects to find in a
`gvisor-bin/` directory next to itself, then extract it to `/usr/local/bin`:

```sh
make release-tarball DESTINATION=bin/
sudo tar -C /usr/local/bin -xf bin/gvisor.tar.bz2
```

To build specific libraries or binaries, you can specify the target:

```sh
make build TARGETS="//pkg/tcpip:tcpip"
```

### Building directly with Bazel (without Docker)

Using Bazel directly isn't recommended due to the extra overhead, but in order
to get started:

-   Look at the [build dockerfile](images/default/Dockerfile) for the canonical
    list of needed dependencies.
-   Install and use [bazelisk][bazelisk]. Otherwise, make sure your bazel
    version matches the one listed in the [.bazelversion](.bazelversion) file.

After setting up dependencies, using Bazel is similar to the Makefile:

```sh
bazel build -c opt //debian:gvisor-release-tar-bz2
```

### Testing

To run standard test suites, you can use:

```sh
make unit-tests
make tests
```

To run specific tests, you can specify the target:

```sh
# Makefile
make test TARGETS="//runsc:version_test"
# Bazel
bazel test //runsc:version_test
```

### Mac OS

Some packages support running tests directly on macOS. At the time of this
writing, gVisor requires bazel 8, which you can install via homebrew:

```sh
brew install bazel@8

# You can then run the tests, e.g.:
$(brew --prefix bazel@8)/bin/bazel test --macos_sdk_version=$(xcrun --show-sdk-version) -- //tools/nogo/... //tools/check{aligned,const,escape,linkname,locks,unsafe}/...
```

### Using `go get`

This project uses [bazel][bazel] to build and manage dependencies. A synthetic
`go` branch is maintained that is compatible with standard `go` tooling for
convenience. This is useful for external packages and libraries that depend on
gVisor subpackages (e.g. userspace networking via Netstack) to import gVisor Go
code into their Go projects.

Select this branch explicitly with the `go` branch query. `@latest` resolves
`master`, which requires Bazel and is not compatible with standard Go tooling:

```sh
go get gvisor.dev/gvisor/pkg/tcpip/transport/tcp@go
```

**NOTE**: **`runsc` builds from this branch are not supported**. gVisor and
`runsc` require several binaries (some of which are not even written in Go) in
order to function. The `go` branch is supported in a best effort capacity, and
direct development on this branch is not supported. Development should occur on
the `master` branch, which is then reflected into the `go` branch.

## Community & Governance

See [GOVERNANCE.md](GOVERNANCE.md) for project governance information.

See [ADOPTERS.md](ADOPTERS.md) for a list of known production users and
adopters.

The [gvisor-users mailing list][gvisor-users-list] and
[gvisor-dev mailing list][gvisor-dev-list] are good starting points for
questions and discussion.

## Security Policy

See [SECURITY.md](SECURITY.md).

## Contributing

See [Contributing.md](CONTRIBUTING.md).

[bazel]: https://bazel.build
[docker]: https://www.docker.com
[gvisor-users-list]: https://groups.google.com/forum/#!forum/gvisor-users
[gvisor-dev]: https://gvisor.dev
[gvisor-dev-list]: https://groups.google.com/forum/#!forum/gvisor-dev
[linux]: https://en.wikipedia.org/wiki/Linux_kernel_interfaces
[oci]: https://www.opencontainers.org
[sandbox]: https://en.wikipedia.org/wiki/Sandbox_(computer_security)
[bazelisk]: https://github.com/bazelbuild/bazelisk
