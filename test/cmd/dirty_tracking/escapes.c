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

// escapes writes application memory through every path by which a write
// reaches a sandbox's memory, one step at a time, for tests that check that
// dirty tracking misses none of them: between steps, every thread is blocked,
// so that two checkpoints taken one after the other see the same memory.
//
//   escapes COMM DATA DIR...
//
// COMM is a directory shared with the test. escapes appends "READY" to
// COMM/out, then, each time it receives SIGUSR1, runs the step named in
// COMM/step and appends "DONE <step> <checksum>" to COMM/out. DATA is a file
// of at least 8 MiB to read. DIRs are directories to write files in (tmpfs,
// or a root filesystem overlay).
//
// Steps:
//   gofer     read(2) DATA into fresh memory; mmap DATA MAP_PRIVATE and
//             MAP_SHARED and read it (page cache fills); write into the
//             MAP_PRIVATE mapping (copy-on-write of file pages)
//   net       TCP over loopback: send 4 MiB and recv it into a buffer; leave
//             64 KiB unread in a socket
//   pipe      write(2)/writev(2) and read(2) through a pipe
//   tmpfs     pwrite(2) a file in each DIR, write through a MAP_SHARED mapping
//             of its first half, ftruncate it down (which zeroes the tail of
//             a page) and up, punch a hole in it
//   fork      fork; the child writes inherited private memory (copy-on-write)
//             and MAP_SHARED anonymous memory, then exits; the parent writes
//             the private memory afterwards (taking ownership without a copy)
//   exec      fork + execve(escapes noop) + wait
//   dontneed  madvise(MADV_DONTNEED) on written private and shared anonymous
//             memory; munmap + mmap at the same address; and MADV_DONTNEED
//             on the private memory of the previous dontneed step, which
//             zeroes it without any write
//   mremap    grow and move a written mapping, write the new part
//   mprotect  write, mprotect read-only, mprotect read-write, write again
//   aio       io_submit(2) reads of DATA into memory
//   reuse     free written memory, wait for its release, map new memory and
//             only read it (it may reuse the decommitted offsets)
//   check     re-read the memory of "reuse": its checksum is its count of
//             non-zero words, which must be 0
//   huge      mmap 256 MiB and write every page
//   churn     all of the above again, with different data
//
// This is the workload of the experiment that showed that no write escapes
// dirty tracking, ported to run under runsc/container's tests.
#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <fcntl.h>
#include <linux/aio_abi.h>
#include <netinet/in.h>
#include <signal.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/uio.h>
#include <sys/wait.h>
#include <unistd.h>

#define MiB (1024 * 1024)
#define PS 4096

static const char* comm;
static const char* data;
static char* const* dirs;
static int ndirs;

static uint64_t seed = 0x9e3779b97f4a7c15ULL;

static uint64_t rnd(void) {
  seed ^= seed << 13;
  seed ^= seed >> 7;
  seed ^= seed << 17;
  return seed;
}

static void fill(void* p, size_t n) {
  uint64_t* w = p;
  for (size_t i = 0; i < n / 8; i++) w[i] = rnd();
}

static uint64_t sum(const void* p, size_t n) {
  const uint64_t* w = p;
  uint64_t s = 0;
  for (size_t i = 0; i < n / 8; i++) s = s * 31 + w[i];
  return s;
}

static void die(const char* m) {
  perror(m);
  exit(1);
}

static void* map(size_t n, int flags) {
  void* p = mmap(NULL, n, PROT_READ | PROT_WRITE, flags, -1, 0);
  if (p == MAP_FAILED) die("mmap");
  return p;
}

// report appends a line to COMM/out.
static void report(const char* format, ...) {
  char path[4096];
  snprintf(path, sizeof path, "%s/out", comm);
  int fd = open(path, O_WRONLY | O_APPEND | O_CREAT | O_CLOEXEC, 0666);
  if (fd < 0) die("open out");
  va_list ap;
  va_start(ap, format);
  vdprintf(fd, format, ap);
  va_end(ap);
  close(fd);
}

static uint64_t gofer(void) {
  uint64_t s = 0;
  int fd = open(data, O_RDONLY);
  if (fd < 0) die("open data");
  char* buf = map(8 * MiB, MAP_PRIVATE | MAP_ANONYMOUS);
  for (size_t off = 0; off < 8 * MiB;) {
    ssize_t n = read(fd, buf + off, 8 * MiB - off);
    if (n <= 0) die("read");
    off += n;
  }
  s += sum(buf, 8 * MiB);
  char* priv = mmap(NULL, 4 * MiB, PROT_READ | PROT_WRITE, MAP_PRIVATE, fd, 0);
  char* shared = mmap(NULL, 4 * MiB, PROT_READ, MAP_SHARED, fd, 4 * MiB);
  if (priv == MAP_FAILED || shared == MAP_FAILED) die("mmap file");
  s += sum(priv, 4 * MiB) + sum(shared, 4 * MiB);
  // Copy-on-write of file pages.
  for (size_t i = 0; i < 4 * MiB; i += 3 * PS) priv[i] ^= 0x5a;
  s += sum(priv, 4 * MiB);
  close(fd);
  // The mappings stay.
  return s;
}

static uint64_t net(void) {
  int ls = socket(AF_INET, SOCK_STREAM, 0), one = 1;
  setsockopt(ls, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);
  struct sockaddr_in a = {.sin_family = AF_INET,
                          .sin_addr.s_addr = htonl(INADDR_LOOPBACK)};
  if (bind(ls, (void*)&a, sizeof a) || listen(ls, 1)) die("bind/listen");
  socklen_t al = sizeof a;
  if (getsockname(ls, (void*)&a, &al)) die("getsockname");
  int c = socket(AF_INET, SOCK_STREAM, 0);
  if (connect(c, (void*)&a, sizeof a)) die("connect");
  int sv = accept(ls, NULL, NULL);
  if (sv < 0) die("accept");
  char* src = map(4 * MiB, MAP_PRIVATE | MAP_ANONYMOUS);
  char* dst = map(4 * MiB, MAP_PRIVATE | MAP_ANONYMOUS);
  fill(src, 4 * MiB);
  size_t sent = 0, got = 0;
  while (got < 4 * MiB) {
    if (sent < 4 * MiB) {
      size_t len = 4 * MiB - sent;
      if (len > 256 * 1024) len = 256 * 1024;
      ssize_t n = send(c, src + sent, len, MSG_DONTWAIT);
      if (n > 0) sent += n;
    }
    ssize_t n = recv(sv, dst + got, 4 * MiB - got, MSG_DONTWAIT);
    if (n > 0) got += n;
  }
  // Left unread in the socket.
  if (send(c, src, 64 * 1024, 0) != 64 * 1024) die("send");
  return sum(dst, 4 * MiB);
}

static uint64_t pipes(void) {
  int p[2];
  if (pipe(p)) die("pipe");
  char* a = map(1 * MiB, MAP_PRIVATE | MAP_ANONYMOUS);
  char* b = map(1 * MiB, MAP_PRIVATE | MAP_ANONYMOUS);
  fill(a, 1 * MiB);
  const size_t ch = 16 * 1024;
  for (size_t off = 0; off < 1 * MiB;) {
    ssize_t w;
    if (off % (2 * ch) == 0) {
      w = write(p[1], a + off, ch);
    } else {
      struct iovec v[2] = {{a + off, ch / 2}, {a + off + ch / 2, ch / 2}};
      w = writev(p[1], v, 2);
    }
    if (w <= 0) die("write/writev pipe");
    for (ssize_t g = 0; g < w;) {
      ssize_t n = read(p[0], b + off + g, w - g);
      if (n <= 0) die("read pipe");
      g += n;
    }
    off += w;
  }
  close(p[0]);
  close(p[1]);
  return sum(b, 1 * MiB);
}

static uint64_t tmpfs(int gen) {
  uint64_t s = 0;
  char* buf = map(8 * MiB, MAP_PRIVATE | MAP_ANONYMOUS);
  fill(buf, 8 * MiB);
  for (int i = 0; i < ndirs; i++) {
    char path[4096];
    if (mkdir(dirs[i], 0755) && errno != EEXIST) die("mkdir");
    snprintf(path, sizeof path, "%s/escapes", dirs[i]);
    int fd = open(path, O_RDWR | O_CREAT, 0644);
    if (fd < 0) die("open file");
    if (pwrite(fd, buf, 8 * MiB, 0) != 8 * MiB) die("pwrite");
    if (pwrite(fd, buf + PS, PS, (gen + 1) * 7 * PS) != PS) die("pwrite2");
    // Only the first half of the file is written through a mapping, so that
    // pages that only pwrite wrote remain, even at a dirty tracking unit
    // larger than the mapping's stride.
    char* m = mmap(NULL, 4 * MiB, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    if (m == MAP_FAILED) die("mmap file");
    for (size_t o = 0; o < 4 * MiB; o += 5 * PS)
      m[o + gen] = (char)(gen * 17 + i);
    s += sum(m, 4 * MiB);
    munmap(m, 4 * MiB);
    // Shrinking the file zeroes the tail of its last page.
    if (ftruncate(fd, 7 * MiB + 100)) die("ftruncate down");
    if (ftruncate(fd, 8 * MiB)) die("ftruncate up");
    // gVisor's tmpfs does not punch holes yet (EOPNOTSUPP).
    if (fallocate(fd, FALLOC_FL_PUNCH_HOLE | FALLOC_FL_KEEP_SIZE, MiB,
                  64 * 1024) &&
        errno != EOPNOTSUPP) {
      die("punch");
    }
    close(fd);
  }
  return s;
}

static char *forkpriv, *forkshared;

static uint64_t forks(int gen) {
  if (!forkpriv) {
    forkpriv = map(8 * MiB, MAP_PRIVATE | MAP_ANONYMOUS);
    forkshared = map(4 * MiB, MAP_SHARED | MAP_ANONYMOUS);
  }
  fill(forkpriv, 8 * MiB);
  fill(forkshared, 4 * MiB);
  pid_t pid = fork();
  if (pid < 0) die("fork");
  if (pid == 0) {
    // Copy-on-write copies.
    for (size_t o = 0; o < 8 * MiB; o += 2 * PS) forkpriv[o] = (char)gen;
    for (size_t o = 0; o < 4 * MiB; o += 3 * PS)
      forkshared[o + 1] = (char)(gen + 1);
    _exit(0);
  }
  int st;
  if (waitpid(pid, &st, 0) != pid) die("waitpid");
  // The child is gone: the parent now holds the only reference.
  for (size_t o = PS; o < 8 * MiB; o += 4 * PS) forkpriv[o] = (char)(gen + 2);
  return sum(forkpriv, 8 * MiB) + sum(forkshared, 4 * MiB);
}

static uint64_t execs(void) {
  pid_t pid = fork();
  if (pid < 0) die("fork");
  if (pid == 0) {
    execl("/proc/self/exe", "escapes", "noop", NULL);
    _exit(127);
  }
  int st;
  if (waitpid(pid, &st, 0) != pid) die("waitpid");
  if (!WIFEXITED(st) || WEXITSTATUS(st) != 0) {
    fprintf(stderr, "exec: status %#x\n", st);
    exit(1);
  }
  return 0;
}

static char* zap_prev;

static uint64_t dontneed(int gen) {
  uint64_t s0 = 0;
  if (zap_prev) {
    // Memory written in an earlier step, zeroed now without any write.
    if (madvise(zap_prev, 8 * MiB, MADV_DONTNEED)) die("dontneed prev");
    s0 = sum(zap_prev, 8 * MiB);
  }
  char* a = map(8 * MiB, MAP_PRIVATE | MAP_ANONYMOUS);
  char* b = map(4 * MiB, MAP_SHARED | MAP_ANONYMOUS);
  zap_prev = a;
  fill(a, 8 * MiB);
  fill(b, 4 * MiB);
  // Private memory reads as zero after this; shared memory keeps its data.
  if (madvise(a + MiB, 2 * MiB, MADV_DONTNEED)) die("dontneed");
  if (madvise(b, MiB, MADV_DONTNEED)) die("dontneed shared");
  // Written again after the zap.
  a[MiB + PS] = (char)gen;
  if (munmap(a + 4 * MiB, MiB)) die("munmap");
  char* r = mmap(a + 4 * MiB, MiB, PROT_READ | PROT_WRITE,
                 MAP_PRIVATE | MAP_ANONYMOUS | MAP_FIXED, -1, 0);
  if (r == MAP_FAILED) die("remap");
  r[100] = (char)gen;
  return s0 + sum(a, 8 * MiB) + sum(b, 4 * MiB);
}

static uint64_t mremaps(int gen) {
  char* a = map(4 * MiB, MAP_PRIVATE | MAP_ANONYMOUS);
  fill(a, 4 * MiB);
  char* b = mremap(a, 4 * MiB, 12 * MiB, MREMAP_MAYMOVE);
  if (b == MAP_FAILED) die("mremap");
  fill(b + 8 * MiB, 4 * MiB);
  b[gen] = (char)gen;
  // A hole to move into.
  char* c = mmap(NULL, 16 * MiB, PROT_NONE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
  if (c == MAP_FAILED) die("mmap hole");
  char* d =
      mremap(b, 12 * MiB, 12 * MiB, MREMAP_MAYMOVE | MREMAP_FIXED, c + 2 * MiB);
  if (d == MAP_FAILED) die("mremap fixed");
  d[5 * PS] = (char)(gen + 3);
  return sum(d, 12 * MiB);
}

static uint64_t mprotects(int gen) {
  char* a = map(2 * MiB, MAP_PRIVATE | MAP_ANONYMOUS);
  fill(a, 2 * MiB);
  if (mprotect(a, 2 * MiB, PROT_READ)) die("mprotect ro");
  uint64_t s = sum(a, 2 * MiB);
  if (mprotect(a, 2 * MiB, PROT_READ | PROT_WRITE)) die("mprotect rw");
  for (size_t o = 0; o < 2 * MiB; o += 3 * PS) a[o] = (char)(gen + 9);
  return s + sum(a, 2 * MiB);
}

static uint64_t aio(void) {
  aio_context_t ctx = 0;
  if (syscall(SYS_io_setup, 8, &ctx)) die("io_setup");
  int fd = open(data, O_RDONLY);
  if (fd < 0) die("open data");
  char* buf = map(2 * MiB, MAP_PRIVATE | MAP_ANONYMOUS);
  struct iocb cbs[8], *ps[8];
  for (int i = 0; i < 8; i++) {
    memset(&cbs[i], 0, sizeof cbs[i]);
    cbs[i].aio_fildes = fd;
    cbs[i].aio_lio_opcode = IOCB_CMD_PREAD;
    cbs[i].aio_buf = (uint64_t)(buf + i * 256 * 1024);
    cbs[i].aio_nbytes = 256 * 1024;
    cbs[i].aio_offset = i * 256 * 1024;
    ps[i] = &cbs[i];
  }
  if (syscall(SYS_io_submit, ctx, 8, ps) != 8) die("io_submit");
  struct io_event ev[8];
  for (int got = 0; got < 8;) {
    long n = syscall(SYS_io_getevents, ctx, 1, 8, ev, NULL);
    if (n < 0) die("io_getevents");
    got += n;
  }
  close(fd);
  // The context, and its ring, stay.
  return sum(buf, 2 * MiB);
}

static char* reused;

static uint64_t nonzero(const char* p, size_t n) {
  uint64_t nz = 0;
  for (size_t o = 0; o < n; o += 8) nz += *(const uint64_t*)(p + o) != 0;
  return nz;
}

static uint64_t reuse(void) {
  // Free written memory, let the Sentry release (decommit) it, then map new
  // memory, which may reuse the same MemoryFile offsets, and only read it.
  char* a = map(16 * MiB, MAP_PRIVATE | MAP_ANONYMOUS);
  fill(a, 16 * MiB);
  munmap(a, 16 * MiB);
  usleep(300 * 1000);
  if (!reused) reused = map(16 * MiB, MAP_PRIVATE | MAP_ANONYMOUS);
  return nonzero(reused, 16 * MiB);
}

static uint64_t check(void) {
  // The read-only reused memory must still be zero.
  return reused ? nonzero(reused, 16 * MiB) : 0;
}

static uint64_t huge(void) {
  char* a = map(256 * MiB, MAP_PRIVATE | MAP_ANONYMOUS);
  for (size_t o = 0; o < 256 * MiB; o += PS) *(uint64_t*)(a + o) = rnd();
  return sum(a, 256 * MiB);
}

// step_requested is set by SIGUSR1's handler.
static volatile sig_atomic_t step_requested;

static void on_usr1(int sig) { step_requested = 1; }

// step reads the name of the step to run from COMM/step into name.
static void step(char* name, size_t size) {
  char path[4096];
  snprintf(path, sizeof path, "%s/step", comm);
  int fd = open(path, O_RDONLY | O_CLOEXEC);
  if (fd < 0) die("open step");
  ssize_t n = read(fd, name, size - 1);
  if (n < 0) die("read step");
  close(fd);
  name[n] = 0;
  name[strcspn(name, "\n")] = 0;
}

int main(int argc, char** argv) {
  if (argc == 2 && !strcmp(argv[1], "noop")) return 0;
  if (argc < 3) {
    fprintf(stderr, "usage: %s COMM DATA DIR...\n", argv[0]);
    return 2;
  }
  comm = argv[1];
  data = argv[2];
  dirs = argv + 3;
  ndirs = argc - 3;

  // Steps run on SIGUSR1. Between steps, the process is blocked in
  // sigsuspend, which a checkpoint interrupts and restarts without returning
  // (unlike sigwaitinfo, which returns EINTR): the process writes no memory
  // between two checkpoints.
  struct sigaction sa = {.sa_handler = on_usr1};
  if (sigaction(SIGUSR1, &sa, NULL)) die("sigaction");
  sigset_t set, waiting;
  sigemptyset(&set);
  sigaddset(&set, SIGUSR1);
  if (sigprocmask(SIG_BLOCK, &set, &waiting)) die("sigprocmask");
  sigdelset(&waiting, SIGUSR1);
  report("READY\n");
  for (int gen = 1;; gen++) {
    while (!step_requested) sigsuspend(&waiting);
    step_requested = 0;
    char name[64];
    step(name, sizeof name);
    uint64_t s;
    if (!strcmp(name, "gofer")) {
      s = gofer();
    } else if (!strcmp(name, "net")) {
      s = net();
    } else if (!strcmp(name, "pipe")) {
      s = pipes();
    } else if (!strcmp(name, "tmpfs")) {
      s = tmpfs(gen);
    } else if (!strcmp(name, "fork")) {
      s = forks(gen);
    } else if (!strcmp(name, "exec")) {
      s = execs();
    } else if (!strcmp(name, "dontneed")) {
      s = dontneed(gen);
    } else if (!strcmp(name, "mremap")) {
      s = mremaps(gen);
    } else if (!strcmp(name, "mprotect")) {
      s = mprotects(gen);
    } else if (!strcmp(name, "aio")) {
      s = aio();
    } else if (!strcmp(name, "reuse")) {
      s = reuse();
    } else if (!strcmp(name, "check")) {
      s = check();
    } else if (!strcmp(name, "huge")) {
      s = huge();
    } else if (!strcmp(name, "churn")) {
      s = gofer() + net() + pipes() + tmpfs(gen) + forks(gen) + execs() +
          dontneed(gen) + mremaps(gen) + mprotects(gen) + aio();
    } else {
      fprintf(stderr, "unknown step %s\n", name);
      return 2;
    }
    report("DONE %s %016llx\n", name, (unsigned long long)s);
  }
}
