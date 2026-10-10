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

// abwriter dirties memory at a set rate while checkpoints pre-copy it, after
// QEMU's "A-B" migration test guest: it rewrites random pages of a buffer,
// each page carrying its index, a generation and a filler derived from both,
// and records each page's generation in a table, so that a restored image can
// be checked for consistency, the pages against the table.
//
//   abwriter [-c | -f FILE] MIB MIB_PER_S COMM
//
// The buffer is MIB MiB of anonymous memory, or of FILE mapped shared (-f).
// Every 10 ms, abwriter rewrites MIB_PER_S / 100 MiB of random pages of it.
// With -c, it also churns memory every 10 ms: it maps, fills, checks and
// unmaps a region; it discards a run of the buffer's pages with
// MADV_DONTNEED, recording them as zero; and every fifth time it forks a
// child, which rewrites pages of its copy of the buffer and exits.
//
// abwriter appends "READY" to COMM/out once the buffer is written. On SIGUSR1,
// it checks every page of the buffer: a page must hold its index, the
// generation that the table records and the filler derived from both, or be
// zero if the table records it discarded. It appends "PASS <pages> <pages
// written>", or "FAIL <bad index> <bad filler> <bad generation>: page <i>
// ..." about the first bad page.
#define _GNU_SOURCE
#include <fcntl.h>
#include <signal.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/mman.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#define PS 4096
#define WORDS (PS / sizeof(uint64_t))
#define TICK_MS 10
// The generation recorded for a discarded page, which reads as zero.
#define DISCARDED UINT64_MAX

static const char* comm;
static uint64_t* buf;
static uint64_t* gen;
static uint64_t npages;
static uint64_t written;
static uint64_t rng = 0x2545f4914f6cdd1dULL;

static void die(const char* m) {
  perror(m);
  exit(1);
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

static uint64_t next_random(void) {
  rng ^= rng << 13;
  rng ^= rng >> 7;
  rng ^= rng << 17;
  return rng;
}

static uint64_t mix(uint64_t x) {
  x ^= x >> 33;
  x *= 0xff51afd7ed558ccdULL;
  x ^= x >> 33;
  return x;
}

static uint64_t filler(uint64_t i, uint64_t g) {
  return mix(i * 0x9e3779b97f4a7c15ULL ^ g);
}

// put writes page i of p with generation g.
static void put(uint64_t* p, uint64_t i, uint64_t g) {
  uint64_t* pg = p + i * WORDS;
  uint64_t f = filler(i, g);
  for (uint64_t k = 2; k < WORDS; k++) pg[k] = f + k;
  pg[1] = g;
  pg[0] = i;
}

// rewrite writes page i of the buffer with a new generation, and records it.
static void rewrite(uint64_t i) {
  uint64_t g = ++written;
  put(buf, i, g);
  gen[i] = g;
}

// check checks every page of the buffer against the table, and reports the
// result.
static void check(void) {
  uint64_t bad_index = 0, bad_filler = 0, bad_gen = 0, first = npages;
  for (uint64_t i = 0; i < npages; i++) {
    const uint64_t* pg = buf + i * WORDS;
    int bad = 0;
    if (gen[i] == DISCARDED) {
      for (uint64_t k = 0; k < WORDS; k++) {
        if (pg[k] != 0) {
          bad_filler++;
          bad = 1;
          break;
        }
      }
    } else if (pg[0] != i) {
      bad_index++;
      bad = 1;
    } else {
      uint64_t f = filler(i, pg[1]);
      for (uint64_t k = 2; k < WORDS; k++) {
        if (pg[k] != f + k) {
          bad_filler++;
          bad = 1;
          break;
        }
      }
      if (pg[1] != gen[i]) {
        bad_gen++;
        bad = 1;
      }
    }
    if (bad && first == npages) first = i;
  }
  if (first == npages) {
    report("PASS %llu %llu\n", (unsigned long long)npages,
           (unsigned long long)written);
    return;
  }
  const uint64_t* pg = buf + first * WORDS;
  report(
      "FAIL %llu %llu %llu: page %llu holds index %llu, generation %llu; "
      "the table %llu\n",
      (unsigned long long)bad_index, (unsigned long long)bad_filler,
      (unsigned long long)bad_gen, (unsigned long long)first,
      (unsigned long long)pg[0], (unsigned long long)pg[1],
      (unsigned long long)gen[first]);
}

// churn allocates, fills, checks and frees a region; discards a run of the
// buffer's pages; and, every fifth call, forks a child that rewrites pages of
// its copy of the buffer.
static void churn(void) {
  static uint64_t calls;
  const uint64_t region_pages = 64;
  uint64_t* region = mmap(NULL, region_pages * PS, PROT_READ | PROT_WRITE,
                          MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
  if (region == MAP_FAILED) die("mmap region");
  uint64_t g = next_random();
  for (uint64_t i = 0; i < region_pages; i++) put(region, i, g);
  for (uint64_t i = 0; i < region_pages; i++) {
    if (region[i * WORDS] != i || region[i * WORDS + 1] != g) {
      report("FAIL churn: region page %llu lost its contents\n",
             (unsigned long long)i);
      exit(1);
    }
  }
  if (munmap(region, region_pages * PS)) die("munmap region");

  const uint64_t run = 16;
  uint64_t start = next_random() % (npages - run);
  if (madvise(buf + start * WORDS, run * PS, MADV_DONTNEED)) die("madvise");
  for (uint64_t i = start; i < start + run; i++) gen[i] = DISCARDED;

  if (calls++ % 5 == 0) {
    pid_t pid = fork();
    if (pid < 0) die("fork");
    if (pid == 0) {
      for (int n = 0; n < 64; n++) put(buf, next_random() % npages, 0);
      _exit(0);
    }
    int status;
    if (waitpid(pid, &status, 0) != pid || !WIFEXITED(status) ||
        WEXITSTATUS(status) != 0) {
      die("waitpid");
    }
  }
}

static void usage(const char* argv0) {
  fprintf(stderr, "usage: %s [-c | -f FILE] MIB MIB_PER_S COMM\n", argv0);
  exit(2);
}

static double now_ms(void) {
  struct timespec t;
  clock_gettime(CLOCK_MONOTONIC, &t);
  return t.tv_sec * 1e3 + t.tv_nsec / 1e6;
}

int main(int argc, char** argv) {
  int churning = 0;
  const char* file = NULL;
  int opt;
  while ((opt = getopt(argc, argv, "cf:")) != -1) {
    switch (opt) {
      case 'c':
        churning = 1;
        break;
      case 'f':
        file = optarg;
        break;
      default:
        usage(argv[0]);
    }
  }
  // Discarding pages of a shared file mapping does not zero them.
  if (argc - optind != 3 || (churning && file)) usage(argv[0]);
  npages = strtoull(argv[optind], NULL, 10) * (1 << 20) / PS;
  double mibps = atof(argv[optind + 1]);
  comm = argv[optind + 2];

  if (file) {
    int fd = open(file, O_RDWR | O_CREAT | O_TRUNC | O_CLOEXEC, 0644);
    if (fd < 0) die("open file");
    if (ftruncate(fd, npages * PS)) die("ftruncate");
    buf = mmap(NULL, npages * PS, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    close(fd);
  } else {
    buf = mmap(NULL, npages * PS, PROT_READ | PROT_WRITE,
               MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
  }
  if (buf == MAP_FAILED) die("mmap buffer");
  gen = mmap(NULL, npages * sizeof *gen, PROT_READ | PROT_WRITE,
             MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
  if (gen == MAP_FAILED) die("mmap table");
  for (uint64_t i = 0; i < npages; i++) rewrite(i);

  sigset_t set;
  sigemptyset(&set);
  sigaddset(&set, SIGUSR1);
  if (sigprocmask(SIG_BLOCK, &set, NULL)) die("sigprocmask");
  report("READY\n");

  // Pages to write per tick, with the fraction carried over.
  const double per_tick = mibps * (1 << 20) / PS * TICK_MS / 1000;
  double due = 0;
  double next = now_ms();
  const struct timespec no_wait = {0};
  for (;;) {
    for (due += per_tick; due >= 1; due--) rewrite(next_random() % npages);
    if (churning) churn();
    if (sigtimedwait(&set, NULL, &no_wait) == SIGUSR1) check();
    next += TICK_MS;
    double wait = next - now_ms();
    if (wait > 0) {
      struct timespec t = {(time_t)(wait / 1e3),
                           (long)((wait - (time_t)(wait / 1e3) * 1e3) * 1e6)};
      nanosleep(&t, NULL);
    } else {
      next = now_ms();
    }
  }
}
