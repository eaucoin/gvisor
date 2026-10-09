# Checkpoint image format, version 2

Status as of 2026-10-09: Implemented (`pkg/sentry/state/checkpointimage`,
`pkg/sentry/pgalloc`).

## Synopsis

A checkpoint image may keep some of its memory in the pages files of other
images, its *layers*, which it names by their identity. One format then serves
full checkpoints, incremental checkpoints, templates shared by many sandboxes,
pre-copy rounds that rewrite pages, and images re-laid out for faster
background restores. This document is the reference of that format: the byte
layout of the pages metadata file, its protos, what an image's identity covers,
how layers are found, how chains are resolved when an image is saved, what a
reader refuses, and how the format is versioned.

How to use images and their layers is described in the
[user guide](../user_guide/checkpoint_restore.md#checkpoint-images-and-their-layers).

## Background

Version 1 of the pages metadata file was a length-prefixed
`MemoryFileMetadataProto` per MemoryFile, with `version = 1`, no magic and no
checksum, and the pages file was positional: no offset was stored, save and
load both walked the known-committed memory accounting ranges in order, adding
their lengths. An image could therefore only hold all of its own pages, in
MemoryFile order. A delta, a pre-copy round that writes a page again, or a
pages file laid out in another order could not be expressed, and a reader could
not tell a file of another version, or a corrupt one, from a valid one before
parsing it.

## Files of an image

An uncompressed checkpoint image is a directory of three files
(`pkg/sentry/state/checkpointfiles`):

*   `checkpoint.img`: the state file, the sandbox's kernel state. It starts with
    JSON metadata (see [State file metadata](#state-file-metadata)).
*   `pages_meta.img`: the pages metadata file, described below.
*   `pages.img`: the pages file, raw page data. It has no header, so that it can
    be read with `O_DIRECT` and by ranges. The data of a page is at the offset
    that an extent gives, page-aligned, in any order. It may hold bytes that no
    extent refers to, such as the earlier copies of pages that a pre-copy round
    wrote again.

A compressed checkpoint keeps its memory in its state file and has neither
`pages_meta.img` nor `pages.img`; nothing in this document applies to it.

## Byte layout of `pages_meta.img`

The file is a header, a body, a trailer and a page hashes section. Integers are
little-endian.

### Header (32 bytes)

| Offset | Size | Field    | Value                                              |
| ------ | ---- | -------- | -------------------------------------------------- |
| 0      | 8    | `magic`  | `gVisorPM`                                         |
| 8      | 2    | `major`  | 2                                                  |
| 10     | 2    | `minor`  | 0                                                  |
| 12     | 4    | `flags`  | 0; no flag is defined                              |
| 16     | 8    | `length` | the length of the body in bytes, at most 1 GiB     |
| 24     | 8    | `crc`    | CRC-64 of bytes 0 to 23                            |

CRC-64 is Go's `hash/crc64` with the ECMA-182 polynomial (CRC-64/XZ).

### Body (`length` bytes)

A sequence of records, each a `uint64` length followed by that many bytes of a
serialized proto (deterministic serialization):

1.  one `ImageProto`;
2.  one `MemoryFileMetadataProto` per MemoryFile: the application MemoryFile's,
    then one per private MemoryFile in `ImageProto.private_memory_files` order.

Nothing follows the last record.

### Trailer (8 bytes)

The CRC-64 of the body.

### Page hashes section

If `ImageProto.page_hash` is 1, one XXH64 hash (seed 0), a `uint64`, of every
known-committed 4 KiB page of every MemoryFile, MemoryFile by MemoryFile in body
order, and within a MemoryFile in offset order; a page without an extent is
zero, and has the hash of a zero page. The section is 8 bytes per known-committed
page, 2 MiB per GiB of memory. Its SHA-256 is `ImageProto.page_hashes_digest`.
The file ends after it. If `page_hash` is 0, the section is empty.

The page hashes follow the trailer because a restore does not need them: only a
save relative to the image, or a check of its pages, does. A restore reads the
header, body and trailer, and reads the page hashes in the background.

## Protos

The protos are in `pkg/sentry/pgalloc/pgalloc.proto`.

`ImageProto` describes the image as a whole:

| Field                  | No. | Meaning                                                                 |
| ---------------------- | --- | ----------------------------------------------------------------------- |
| `layers`               | 1   | `LayerProto`s; `layers[0]` is the image itself, the others are images that hold some of its pages, in order of first reference by an extent |
| `working_set`          | 2   | `WorkingSetProto`: the pages a sandbox touched first after a restore, in the order it touched them; optional |
| `page_hash`            | 3   | the hash function of the page hashes: 0 for none, 1 for XXH64 (seed 0) of each 4 KiB page |
| `private_memory_files` | 4   | `ResourceIDProto`s (`container_name`, `path`): the owners of the private MemoryFiles whose records follow the application MemoryFile's, in that order |
| `page_hashes_digest`   | 5   | the SHA-256 of the page hashes section if `page_hash` is not 0, and empty otherwise |

`LayerProto` identifies a layer:

| Field        | No. | Meaning                                                                 |
| ------------ | --- | ----------------------------------------------------------------------- |
| `digest`     | 1   | the layer's identity (32 bytes); empty for layer 0                       |
| `pages_size` | 2   | the size of the layer's `pages.img` in bytes, page-aligned               |

`MemoryFileMetadataProto` has `version = 2` and keeps the fields of version 1
(`chunks`, `mem_acct`, `unfree_small`, `unfree_huge`, `unwaste_small`,
`unwaste_huge`, `subreleased`, fields 2 to 8), with two more:

| Field         | No. | Meaning                                                                 |
| ------------- | --- | ----------------------------------------------------------------------- |
| `extents`     | 9   | `ExtentProto`s that map known-committed memory to its data, sorted by `start`; a known-committed page without an extent is zero |
| `page_hashes` | 10  | empty in a pages metadata file: its page hashes are in the page hashes section |

`ExtentProto` maps a range of a MemoryFile to the pages file of one layer:

| Field    | No. | Meaning                                                                 |
| -------- | --- | ----------------------------------------------------------------------- |
| `start`  | 1   | the first MemoryFile offset of the range                                  |
| `end`    | 2   | the MemoryFile offset that ends the range (exclusive)                     |
| `layer`  | 3   | an index into `ImageProto.layers`                                       |
| `offset` | 4   | the offset of `start`'s data in that layer's `pages.img`                 |

`WorkingSetProto` has `version = 1`, `unit` (the recording granularity in
bytes, a power of two of at least 4 KiB), `window_ns` (how long the set was
recorded for) and `extents`, ranges of the application MemoryFile in
first-touch order.

## Image identity

An image's identity is the SHA-256 of the header, body and trailer of its
`pages_meta.img`, that is, of the file up to its page hashes section. It covers
every extent, and so where every page of the image is; the identity of every
layer, and so, transitively, the data the image refers to in them; and, through
`page_hashes_digest`, the hash of every page. Two images with different memory
have different identities.

The identity identifies; it does not protect. The page hashes are
non-cryptographic, and an image's pages file is not hashed as a whole, since
that would mean reading all of it to restore any of it. A reader checks the
pages it reads against their hashes only on request (`runsc image verify
--pages`). The state file's HMAC key (`statefile.NewWriter`) remains the
integrity mechanism for those who need one.

The identity is written in lowercase hexadecimal where it names files or
directories, and printed by `runsc image inspect`.

## Layers and how they are found

An image refers to the pages file of layer *i* through the extents whose
`layer` is *i*. When it is restored, runsc looks for each layer *i* ≥ 1 with
digest *d* in, in order:

1.  `<image>/layers/<d>/`, a directory holding the layer's `pages_meta.img` and
    `pages.img`, or a symbolic link to one;
2.  for each `--layer-path` *p*, in order: *p* itself, if it is an image
    directory, then *p*`/<d>/`.

A candidate is the layer only if the identity of its `pages_meta.img` is *d*;
its `pages.img` must then be `pages_size` bytes long. runsc opens the pages
file of every layer and passes them to the Sentry in layer order, and fails
before starting the sandbox if a layer is missing or does not match. Images
read through a checkpoint gofer find their layers as the objects
`layers/<d>/pages_meta.img` and `layers/<d>/pages.img` under the image's prefix,
and the Sentry checks their identity itself.

Nothing in a layer refers to the images that use it, so any image can be the
layer of any number of others: a template can be the parent of every sandbox
started from it, and is stored once.

## Chain resolution at save

An image saved relative to another, its parent (`pgalloc.SaveOpts.Base`),
starts with the parent's layers as its own layers 1 to *n* (the parent is one of
them) and refers to them directly:

*   a page that has not been written since the parent was saved
    (`SaveOpts.Clean`), or whose XXH64 is the parent's hash at that offset,
    refers to the parent's extent for that page, with its layer index remapped
    to the new image's (`SaveOpts.BaseLayers`), so it names the layer that
    holds the data, not the parent;
*   a page that is zero has no extent;
*   any other page is written to the image's own `pages.img`, and its extent
    refers to layer 0.

`checkpointimage.Writer.Finish` then drops the layers that no extent refers to
and orders the others by first reference, renumbering the extents. Every range
of memory is therefore read from the layer that holds it in one hop, however
long the chain of saves that produced it, and a chain shortens itself as pages
are written over: an image whose pages were all rewritten by later saves is no
longer a layer of their images.

A page that was written since the parent but whose XXH64 equals the parent's
hash at that offset is saved as unchanged. For data that is not chosen to
collide, a changed page has that hash with a probability of 2^-64; an
application that crafts a collision can only make its own memory restore with
the parent's contents of that page, which it could have written itself.

## Validation

A reader checks the header and the trailer before it parses anything in the
body, and refuses the file (`checkpointimage.ErrFormat`) on any of:

*   a bad magic, a header CRC that does not match, a major version other than
    its own, a minor version above its own, a flag it does not know, or a body
    longer than 1 GiB;
*   a body shorter than the header says, or a body CRC that does not match;
*   a record longer than what remains of the body, a proto that does not parse,
    or bytes after the last record;
*   no layers, a digest for layer 0, a layer digest that is not 32 bytes, a
    duplicate layer digest, or a `pages_size` that is not page-aligned;
*   an unknown `page_hash`, or a `page_hashes_digest` that is not 32 bytes when
    there are page hashes, or not empty when there are none;
*   a working set of another version, a unit that is not a power of two of at
    least 4 KiB, or an extent that is empty or not page-aligned;
*   a `MemoryFileMetadataProto` of a version other than 2;
*   memory accounting ranges that are empty, not page-aligned, unsorted or
    overlapping;
*   extents that are empty, not page-aligned, unsorted or overlapping, outside
    known-committed memory, in a layer that does not exist, or beyond the
    `pages_size` of their layer;
*   page hashes in a record;
*   when it reads the page hashes section: a section of another length than one
    hash per known-committed page, a SHA-256 other than `page_hashes_digest`,
    data after it, or a section larger than 2 GiB (that of 1 TiB of memory).

The writer runs the same validation on what it is about to write, so that a
save fails rather than writes an image that cannot be restored. Everything a
reader accepts is consistent: Go native fuzzing of the header, of whole files
and of bodies past their checksums (2.2 to 2.6 million inputs in 60 s each)
found no input that was both accepted and inconsistent.

## Versioning

As Firecracker does for its snapshots
([`docs/snapshotting/versioning.md`](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/versioning.md)),
the format has its own version, independent of runsc's, in the file itself,
`major.minor`:

*   A reader refuses any major version other than its own, and minor versions
    above its own; it reads lower minor versions.
*   A change that a reader of the current version would misread, or that
    changes the meaning or layout of anything existing, increments the major
    version.
*   A change that a reader of the current version must refuse rather than
    ignore, but that leaves older files readable as they are (a new flag, a new
    `page_hash` function, a new kind of layer), increments the minor version.
*   A proto field that a reader may ignore without misreading the image needs
    no new version: proto3 readers ignore fields they do not know.

`MemoryFileMetadataProto.version` (2) and `WorkingSetProto.version` (1) version
their protos within the format, and readers refuse other versions.

The state file of a checkpoint records the runsc version that saved it, and a
runsc binary restores only checkpoints saved by its own version, so the format
version matters to the tools that read images, and to readers of other builds,
more than to restore.

## State file metadata

The JSON metadata at the start of `checkpoint.img` holds, among others:

| Key             | Value                                                                    |
| --------------- | ------------------------------------------------------------------------ |
| `runsc_version` | the version of runsc that saved the checkpoint                           |
| `image_format`  | the version of the pages metadata format, e.g. `2.0`; absent for compressed checkpoints, which have no pages metadata file |

The image's identity and the size of its pages file are not in the state file's
metadata: the metadata is written before the object graph, and the pages
metadata file, whose SHA-256 is the identity and which holds that size, is
completed after both, once every page is written. Tools read them from
`pages_meta.img`, whose header, body and trailer they read in a few
microseconds.

## Design rationale

**Parents by identity, not a content-addressed store.** Content-hashing the
4 KiB pages of independently started workspaces' checkpoints found that they
share 0 to 3 % of their pages: heap pages hold pointers, and every process lays
out its heap at its own randomized addresses. Fixed chunks of 64 KiB, 512 KiB
and 4 MiB deduplicate 1 %, 0 % and 0 % across images, and content-addressing
full checkpoints in such chunks stores 2.3 to 2.9 times more than 4 KiB
positional deltas, since a small change touches pages scattered over memory.
What sharing there is comes from a common parent: 83 % of the pages of a Python
REPL with numpy and pandas imported are still identical, at the same offset,
after a session's next steps, and 87 to 95 % of the images of npm and Vite
workspaces after their install or start. A parent named by identity, which any
image can be, gives that sharing without a hash index, garbage collection of
chunks or a request per chunk.

**Chains resolved at save.** A restore reads each range from its layer
directly, as E2B's resolved range-to-layer map does, rather than walking parent
after parent as CRIU's `PE_PARENT` pagemap entries do. Depth then costs open
files, not reads: a prototype restored chains of 2 to 15 layers in 0.12 to
0.52 s (median 0.15 s, while hashing every page to check it).

**Page hashes refine deltas.** Almost half of what dirty tracking reports as
written is written back unchanged (3.7 MiB of 4 KiB-tracked dirty pages for 2.0 MiB that
changed, after a small exec); comparing hashes with the parent's saves those
pages as unchanged without reading the parent's data. Hashing is done in
parallel with the page writes, which take longer: 153 ms per GiB on four
goroutines, against 0.6 to 1.3 s per GiB to write the pages. Checkpoint pauses
and restore times measured with and without the format were within the noise of
the shared bench host.

**Page hashes after the trailer.** With the page hashes in the MemoryFile
records, reading the metadata of images of 64 MiB, 512 MiB and 1 GiB took 0.25,
1.7 and 3.3 ms (CRC-64, SHA-256 and unmarshalling of the hashes), twice per
restore, by runsc and by the Sentry, from a cold page cache. In their own
section after the trailer, reading the metadata takes 3.8 µs at every size,
and the fixed costs of a restore (runsc to the restore RPC, the RPC to the pages
files being set up, the application MemoryFile's metadata) went from 14.4,
35.5 and 46.0 ms to 6.3, 11.1 and 9.0 ms, against 9.2, 9.1 and 11.5 ms for
version 1.

**A header and checksums.** A header with a magic, a version and a CRC-64,
checked before anything is parsed, is Firecracker's snapshot discipline; it
lets a reader refuse a file of another version, or a corrupt one, before it
parses it.

**A pages file without a header.** Data at offsets given by extents, in any
order, is QEMU's mapped-ram idea: a pre-copy round writes a page again at a new
offset and updates its extent, and a tool can lay out the working set first so
that a restore's sequential background read loads it first. Without a header,
the pages file stays readable with `O_DIRECT` and by byte ranges from object
stores.
