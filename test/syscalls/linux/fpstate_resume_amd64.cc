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

// This test verifies that threads keep their registers, the vector registers
// included, across system calls and page faults while they are repeatedly
// stopped and continued (and, in save/restore test variants, checkpointed).
// Stopping a thread interrupts it wherever it is, including while it is being
// resumed after a system call or a fault.

#include <signal.h>
#include <sys/mman.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <unistd.h>

#include <atomic>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <memory>
#include <vector>

#include "gtest/gtest.h"
#include "absl/time/clock.h"
#include "absl/time/time.h"
#include "test/util/logging.h"
#include "test/util/save_util.h"
#include "test/util/test_util.h"
#include "test/util/thread_util.h"

namespace gvisor {
namespace testing {

namespace {

constexpr int kThreads = 2;
constexpr absl::Duration kDuration = absl::Seconds(10);

// Registers is the state checked around each system call or fault: all 16
// vector registers and the callee-saved r12-r15.
struct alignas(16) Registers {
  uint8_t xmm[16][16];
  uint64_t r12_r15[4];
};

// Fill sets r to values derived from seed, different for nearby seeds, so that
// registers restored from the state of an earlier check show.
void Fill(Registers* r, uint64_t seed) {
  uint8_t* b = &r->xmm[0][0];
  for (size_t i = 0; i < sizeof(r->xmm); i++) {
    b[i] = static_cast<uint8_t>(seed * 131 + i * 7 + 1) | 1;
  }
  for (int i = 0; i < 4; i++) {
    r->r12_r15[i] = seed * 0x9e3779b97f4a7c15 + i + 1;
  }
}

#define LOAD_XMM(n) "movdqu " #n "*16(%[in]), %%xmm" #n "\n"
#define STORE_XMM(n) "movdqu %%xmm" #n ", " #n "*16(%[out])\n"
#define LOAD_REGISTERS \
  LOAD_XMM(0) LOAD_XMM(1) LOAD_XMM(2) LOAD_XMM(3) LOAD_XMM(4)          \
  LOAD_XMM(5) LOAD_XMM(6) LOAD_XMM(7) LOAD_XMM(8) LOAD_XMM(9)          \
  LOAD_XMM(10) LOAD_XMM(11) LOAD_XMM(12) LOAD_XMM(13) LOAD_XMM(14)     \
  LOAD_XMM(15)                                                         \
  "movq 256(%[in]), %%r12\n"                                           \
  "movq 264(%[in]), %%r13\n"                                           \
  "movq 272(%[in]), %%r14\n"                                           \
  "movq 280(%[in]), %%r15\n"
#define STORE_REGISTERS \
  STORE_XMM(0) STORE_XMM(1) STORE_XMM(2) STORE_XMM(3) STORE_XMM(4)     \
  STORE_XMM(5) STORE_XMM(6) STORE_XMM(7) STORE_XMM(8) STORE_XMM(9)     \
  STORE_XMM(10) STORE_XMM(11) STORE_XMM(12) STORE_XMM(13)              \
  STORE_XMM(14) STORE_XMM(15)                                          \
  "movq %%r12, 256(%[out])\n"                                          \
  "movq %%r13, 264(%[out])\n"                                          \
  "movq %%r14, 272(%[out])\n"                                          \
  "movq %%r15, 280(%[out])\n"
#define CLOBBERED_REGISTERS                                             \
  "memory", "r12", "r13", "r14", "r15", "xmm0", "xmm1", "xmm2", "xmm3", \
      "xmm4", "xmm5", "xmm6", "xmm7", "xmm8", "xmm9", "xmm10", "xmm11", \
      "xmm12", "xmm13", "xmm14", "xmm15"

// AroundSyscall loads the registers from in, calls getppid(2) with a
// "mov $nr, %eax; syscall" sequence (which systrap patches into a call to its
// fast path), and stores the registers to out.
void AroundSyscall(const Registers* in, Registers* out) {
  asm volatile(LOAD_REGISTERS
               "movl %[nr], %%eax\n"
               "syscall\n" STORE_REGISTERS
               :
               : [in] "r"(in), [out] "r"(out), [nr] "i"(SYS_getppid)
               : CLOBBERED_REGISTERS, "rax", "rcx", "r11");
}

// AroundFault loads the registers from in, writes val to *addr (the first
// access to a page: a page fault), and stores the registers to out.
void AroundFault(const Registers* in, Registers* out, uint64_t* addr,
                 uint64_t val) {
  asm volatile(LOAD_REGISTERS "movq %[val], (%[addr])\n" STORE_REGISTERS
               :
               : [in] "r"(in), [out] "r"(out), [addr] "r"(addr), [val] "r"(val)
               : CLOBBERED_REGISTERS);
}

// Check returns whether out has the registers of in, and reports those that
// differ.
bool Check(const char* what, uint64_t seed, const Registers& in,
           const Registers& out) {
  bool ok = true;
  for (int i = 0; i < 16; i++) {
    if (memcmp(in.xmm[i], out.xmm[i], 16) != 0) {
      uint64_t want[2], got[2];
      memcpy(want, in.xmm[i], 16);
      memcpy(got, out.xmm[i], 16);
      fprintf(stderr, "%s %lu: xmm%d is %016lx%016lx, want %016lx%016lx\n",
              what, seed, i, got[1], got[0], want[1], want[0]);
      ok = false;
    }
  }
  for (int i = 0; i < 4; i++) {
    if (in.r12_r15[i] != out.r12_r15[i]) {
      fprintf(stderr, "%s %lu: r%d is %016lx, want %016lx\n", what, seed,
              12 + i, out.r12_r15[i], in.r12_r15[i]);
      ok = false;
    }
  }
  return ok;
}

// CheckRegisters checks registers around system calls and page faults until
// deadline, and returns the number of failed checks.
int CheckRegisters(uint64_t first_seed, absl::Time deadline) {
  int failed = 0;
  Registers in, out;
  for (uint64_t seed = first_seed; absl::Now() < deadline; seed += 2) {
    Fill(&in, seed);
    memset(&out, 0, sizeof(out));
    AroundSyscall(&in, &out);
    failed += !Check("syscall", seed, in, out);

    // Map the page as glibc maps a new malloc arena: a PROT_NONE reservation
    // of which the start is made accessible.
    void* p = mmap(nullptr, 2 * kPageSize, PROT_NONE,
                   MAP_PRIVATE | MAP_ANONYMOUS | MAP_NORESERVE, -1, 0);
    TEST_PCHECK(p != MAP_FAILED);
    TEST_PCHECK(mprotect(p, kPageSize, PROT_READ | PROT_WRITE) == 0);
    uint64_t* addr = static_cast<uint64_t*>(p);
    Fill(&in, seed + 1);
    memset(&out, 0, sizeof(out));
    AroundFault(&in, &out, addr, seed + 1);
    failed += !Check("fault", seed + 1, in, out);
    TEST_CHECK(*addr == seed + 1);
    TEST_PCHECK(munmap(p, 2 * kPageSize) == 0);
  }
  return failed;
}

TEST(FPStateResumeTest, StopAndContinue) {
  absl::Time deadline = absl::Now() + kDuration;
  pid_t child = fork();
  if (child == 0) {
    std::atomic<int> failed(0);
    {
      std::vector<std::unique_ptr<ScopedThread>> threads;
      for (int i = 0; i < kThreads; i++) {
        threads.push_back(std::make_unique<ScopedThread>([&, i] {
          failed +=
              CheckRegisters(1 + (static_cast<uint64_t>(i) << 32), deadline);
        }));
      }
    }
    _exit(failed.load() == 0 ? 0 : 1);
  }
  ASSERT_THAT(child, SyscallSucceeds());

  // Stop and continue the child until it exits; checkpoint it too in
  // save/restore test variants.
  int status;
  while (true) {
    ASSERT_THAT(kill(child, SIGSTOP), SyscallSucceeds());
    ASSERT_THAT(RetryEINTR(waitpid)(child, &status, WUNTRACED),
                SyscallSucceedsWithValue(child));
    if (!WIFSTOPPED(status)) {
      break;
    }
    ASSERT_THAT(kill(child, SIGCONT), SyscallSucceeds());
    MaybeSave();
  }
  EXPECT_TRUE(WIFEXITED(status) && WEXITSTATUS(status) == 0)
      << "status " << status;
}

}  // namespace

}  // namespace testing
}  // namespace gvisor
