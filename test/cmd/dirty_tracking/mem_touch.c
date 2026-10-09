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

// mem_touch is CRIU's zdtm test mem-touch: it writes, without pause, an
// increasing counter at the start of random pages of a mapping, and records
// each value written in a shadow table, while checkpoints and restores happen.
// On SIGUSR1 it checks that every page holds the value the shadow table
// recorded for it.
//
//   mem_touch COMM SHADOW
//
// mem_touch appends "READY" to COMM/out once it runs, and the result of each
// check: "PASS <values written>", or "FAIL <pages that differ>" followed by
// the first of them.
//
// Unlike CRIU's, whose shadow table is an array on the stack, the shadow table
// is the file SHADOW (in tmpfs), written with pwrite(2): a write that dirty
// tracking misses is then detected whichever of the two paths, the
// application's stores or the Sentry's writes, misses it, rather than lost on
// both sides alike.
#define _GNU_SOURCE
#include <fcntl.h>
#include <signal.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/mman.h>
#include <time.h>
#include <unistd.h>

#define PAGES 4096
#define PS 4096

static const char* comm;

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

int main(int argc, char** argv) {
  if (argc != 3) {
    fprintf(stderr, "usage: %s COMM SHADOW\n", argv[0]);
    return 2;
  }
  comm = argv[1];
  char* mem = mmap(NULL, PAGES * PS, PROT_READ | PROT_WRITE,
                   MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
  if (mem == MAP_FAILED) die("mmap");
  int shadow = open(argv[2], O_RDWR | O_CREAT | O_TRUNC | O_CLOEXEC, 0644);
  if (shadow < 0) die("open shadow");
  if (ftruncate(shadow, PAGES * sizeof(uint32_t))) die("ftruncate");

  sigset_t set;
  sigemptyset(&set);
  sigaddset(&set, SIGUSR1);
  if (sigprocmask(SIG_BLOCK, &set, NULL)) die("sigprocmask");
  report("READY\n");

  uint64_t seed = 0x9e3779b97f4a7c15ULL;
  const struct timespec pause = {.tv_nsec = 100 * 1000};
  const struct timespec poll = {0};
  for (uint32_t rover = 1;; rover++) {
    seed ^= seed << 13;
    seed ^= seed >> 7;
    seed ^= seed << 17;
    uint32_t pfn = seed % PAGES;
    *(volatile uint32_t*)(mem + (size_t)pfn * PS) = rover;
    if (pwrite(shadow, &rover, sizeof rover, pfn * sizeof rover) !=
        sizeof rover)
      die("pwrite");
    nanosleep(&pause, NULL);

    if (sigtimedwait(&set, NULL, &poll) != SIGUSR1) continue;
    static uint32_t want[PAGES];
    if (pread(shadow, want, sizeof want, 0) != sizeof want) die("pread");
    int bad = 0;
    uint32_t first = 0;
    for (uint32_t i = 0; i < PAGES; i++) {
      if (*(volatile uint32_t*)(mem + (size_t)i * PS) != want[i]) {
        if (!bad++) first = i;
      }
    }
    if (bad) {
      report("FAIL %d: page %u holds %u, the shadow table %u\n", bad, first,
             *(volatile uint32_t*)(mem + (size_t)first * PS), want[first]);
    } else {
      report("PASS %u\n", rover);
    }
  }
}
