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

// dirtier: a workload that holds N MiB of anonymous memory, dirties a
// controlled share of it every second, and reports, from inside the sandbox,
// how long it was stopped.
//
//   dirtier MIB DIRTY_PCT_PER_S [BURST_HZ=10] [GAP_MS=10]
//
// Three threads, all writing whole lines to stdout:
//
// - main allocates and touches MIB MiB ("READY <ms to touch>"), then serves
//   commands read from stdin, one per line: "ping X" answers "PONG X";
//   "touch" writes every page and answers "TOUCHED <ms>"; "quit" exits.
// - dirtier, BURST_HZ times a second, writes the next
//   MIB * DIRTY_PCT / 100 / BURST_HZ pages (cycling through the buffer), and
//   once a second prints "STAT <bursts> <pages> <p50 us> <max us>" for the
//   bursts of that second.
// - ticker sleeps 1 ms in a loop and prints "GAP <ms>" whenever
//   CLOCK_MONOTONIC advanced by more than GAP_MS between two wake-ups: the
//   workload was stopped (checkpoint pause, restore downtime, a demand fault,
//   the host).
//
// Build: gcc -O2 -static -pthread -o dirtier dirtier.c

#define _GNU_SOURCE
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <time.h>
#include <unistd.h>

static uint8_t* buf;
static size_t npages, pagesz;
static double dirty_pct;
static int burst_hz = 10;
static double gap_ms = 10;

static double now_ms(void) {
  struct timespec ts;
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return ts.tv_sec * 1e3 + ts.tv_nsec / 1e6;
}

static void sleep_ms(double ms) {
  struct timespec ts = {(time_t)(ms / 1e3),
                        (long)((ms - (time_t)(ms / 1e3) * 1e3) * 1e6)};
  nanosleep(&ts, NULL);
}

static int cmp_double(const void* a, const void* b) {
  double x = *(const double*)a, y = *(const double*)b;
  return (x > y) - (x < y);
}

static void* ticker(void* arg) {
  (void)arg;
  double prev = now_ms();
  for (;;) {
    sleep_ms(1);
    double t = now_ms();
    if (t - prev > gap_ms) printf("GAP %.1f\n", t - prev);
    prev = t;
  }
  return NULL;
}

static void* dirtier(void* arg) {
  (void)arg;
  size_t per_burst = (size_t)(npages * dirty_pct / 100.0 / burst_hz);
  size_t next = 0;
  uint8_t gen = 1;
  double lat[1024];
  double period = 1000.0 / burst_hz, start = now_ms(), report = start + 1000;
  int n = 0;
  size_t pages = 0;
  if (per_burst == 0) return NULL;
  for (;;) {
    double t0 = now_ms();
    for (size_t i = 0; i < per_burst; i++) {
      buf[next * pagesz + (gen % 64) * 8] = gen;
      if (++next == npages) {
        next = 0;
        gen++;
      }
    }
    double t1 = now_ms();
    if (n < 1024) lat[n++] = (t1 - t0) * 1e3;
    pages += per_burst;
    if (t1 >= report) {
      qsort(lat, n, sizeof lat[0], cmp_double);
      printf("STAT %d %zu %.0f %.0f\n", n, pages, lat[n / 2], lat[n - 1]);
      n = 0;
      pages = 0;
      while (report <= t1) report += 1000;
    }
    start += period;
    double wait = start - now_ms();
    if (wait > 0) {
      sleep_ms(wait);
    } else {
      start = now_ms();  // Fell behind (stopped): do not burst to catch up.
    }
  }
  return NULL;
}

int main(int argc, char** argv) {
  if (argc < 3) {
    fprintf(stderr, "usage: %s MIB DIRTY_PCT_PER_S [BURST_HZ] [GAP_MS]\n",
            argv[0]);
    return 2;
  }
  setvbuf(stdout, NULL, _IOLBF, 0);
  size_t mib = strtoul(argv[1], NULL, 10);
  dirty_pct = atof(argv[2]);
  if (argc > 3) burst_hz = atoi(argv[3]);
  if (argc > 4) gap_ms = atof(argv[4]);
  pagesz = sysconf(_SC_PAGESIZE);
  npages = mib * 1048576 / pagesz;
  buf = mmap(NULL, npages * pagesz, PROT_READ | PROT_WRITE,
             MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
  if (buf == MAP_FAILED) {
    perror("mmap");
    return 1;
  }
  double t0 = now_ms();
  // Pseudo-random contents (xorshift64): no zero pages, and as incompressible
  // as real data is at worst, so that compressed checkpoints are not flattered.
  uint64_t x = 0x9e3779b97f4a7c15ULL, *w = (uint64_t*)buf;
  for (size_t i = 0; i < npages * pagesz / 8; i++) {
    x ^= x << 13;
    x ^= x >> 7;
    x ^= x << 17;
    w[i] = x;
  }
  printf("READY %.0f\n", now_ms() - t0);

  pthread_t th;
  pthread_create(&th, NULL, ticker, NULL);
  pthread_create(&th, NULL, dirtier, NULL);

  char line[256];
  while (fgets(line, sizeof line, stdin)) {
    if (!strncmp(line, "ping", 4)) {
      printf("PONG%s", line + 4);
    } else if (!strncmp(line, "touch", 5)) {
      double s = now_ms();
      for (size_t p = 0; p < npages; p++) buf[p * pagesz + pagesz - 1]++;
      printf("TOUCHED %.1f\n", now_ms() - s);
    } else if (!strncmp(line, "quit", 4)) {
      break;
    }
  }
  return 0;
}
