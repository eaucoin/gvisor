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

// Tests of the host kernel's userfaultfd write-protection in asynchronous mode
// and PAGEMAP_SCAN, used as runsc's --dirty-tracking=uffd uses them
// (pkg/sentry/hostmm, pkg/sentry/pgalloc, pkg/sentry/platform/systrap): they
// check, on the host they run on, each behaviour that write tracking relies
// on. One memfd is mapped shared by this process, as the Sentry maps
// MemoryFiles, and by two child processes that share its file descriptor
// table, as systrap's stubs share the Sentry's. Each child creates the
// userfaultfd of its own address space; this process registers and
// write-protects the child's mapping with it, and reads which pages the child
// wrote from the child's pagemap, which it opens through a procfs directory
// FD, as the Sentry does.
//
// The tests run on the host only, not in gVisor. They are skipped where the
// host cannot track writes (Linux 6.7 and later can), or fail there if the
// environment variable GVISOR_REQUIRE_UFFD_WP is 1.

#include <errno.h>
#include <fcntl.h>
#include <linux/userfaultfd.h>
#include <poll.h>
#include <sched.h>
#include <signal.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

#include <string>
#include <vector>

#include "gmock/gmock.h"
#include "gtest/gtest.h"
#include "absl/strings/str_cat.h"
#include "absl/strings/str_split.h"
#include "test/util/file_descriptor.h"
#include "test/util/posix_error.h"
#include "test/util/test_util.h"

// The parts of Linux 6.7's UAPI that the build's headers may lack.
#ifndef UFFD_USER_MODE_ONLY
#define UFFD_USER_MODE_ONLY 1
#endif
#ifndef UFFD_FEATURE_WP_UNPOPULATED
#define UFFD_FEATURE_WP_UNPOPULATED (1 << 13)
#endif
#ifndef UFFD_FEATURE_WP_ASYNC
#define UFFD_FEATURE_WP_ASYNC (1 << 15)
#endif
#ifndef SYS_pidfd_open
#define SYS_pidfd_open 434
#endif

namespace gvisor {
namespace testing {

namespace {

// PAGEMAP_SCAN, from include/uapi/linux/fs.h.
struct PageRegion {
  uint64_t start;
  uint64_t end;
  uint64_t categories;
};

struct PmScanArg {
  uint64_t size;
  uint64_t flags;
  uint64_t start;
  uint64_t end;
  uint64_t walk_end;
  uint64_t vec;
  uint64_t vec_len;
  uint64_t max_pages;
  uint64_t category_inverted;
  uint64_t category_mask;
  uint64_t category_anyof_mask;
  uint64_t return_mask;
};

constexpr unsigned long kPagemapScan = _IOWR('f', 16, PmScanArg);
constexpr uint64_t kPmScanWpMatching = 1 << 0;
constexpr uint64_t kPmScanCheckWpasync = 1 << 1;
constexpr uint64_t kPageIsWritten = 1 << 1;

// The flags and features of write tracking's userfaultfds
// (hostmm.UserfaultfdFlags and hostmm.WPAsyncFeatures).
constexpr int kUserfaultfdFlags = O_CLOEXEC | O_NONBLOCK | UFFD_USER_MODE_ONLY;
constexpr uint64_t kWPAsyncFeatures =
    UFFD_FEATURE_WP_ASYNC | UFFD_FEATURE_WP_UNPOPULATED;

// The number of pages of the memfd.
constexpr int kPages = 32;

size_t MappingSize() { return kPages * kPageSize; }

// UserfaultfdAPI performs the UFFDIO_API handshake of write tracking on uffd.
PosixError UserfaultfdAPI(int uffd) {
  struct uffdio_api api = {};
  api.api = UFFD_API;
  api.features = kWPAsyncFeatures;
  if (ioctl(uffd, UFFDIO_API, &api) < 0) {
    return PosixError(errno, "UFFDIO_API");
  }
  if ((api.features & kWPAsyncFeatures) != kWPAsyncFeatures) {
    return PosixError(
        EINVAL, absl::StrCat("UFFDIO_API: features ", api.features, " lack ",
                             kWPAsyncFeatures));
  }
  return NoError();
}

// WriteTrackingAvailable returns an error if this host cannot track writes.
PosixError WriteTrackingAvailable() {
  int uffd = syscall(SYS_userfaultfd, kUserfaultfdFlags);
  if (uffd < 0) {
    return PosixError(errno, "userfaultfd");
  }
  FileDescriptor fd(uffd);
  return UserfaultfdAPI(fd.get());
}

// WriteProtect registers [start, start+MappingSize()) of the address space of
// uffd for write-protection, and write-protects it (hostmm.WriteProtectRange).
PosixError WriteProtect(int uffd, uintptr_t start) {
  struct uffdio_register reg = {};
  reg.range.start = start;
  reg.range.len = MappingSize();
  reg.mode = UFFDIO_REGISTER_MODE_WP;
  if (ioctl(uffd, UFFDIO_REGISTER, &reg) < 0) {
    return PosixError(errno, "UFFDIO_REGISTER");
  }
  struct uffdio_writeprotect wp = {};
  wp.range.start = start;
  wp.range.len = MappingSize();
  wp.mode = UFFDIO_WRITEPROTECT_MODE_WP;
  if (ioctl(uffd, UFFDIO_WRITEPROTECT, &wp) < 0) {
    return PosixError(errno, "UFFDIO_WRITEPROTECT");
  }
  return NoError();
}

// Harvest returns the pages of the mapping at start written since they were
// write-protected, in the address space of the pagemap file pagemap, and
// write-protects them again, as write tracking's harvests do
// (hostmm.PagemapScanBuf.HarvestWritten): with the masks of PAGEMAP_SCAN's
// fast path only.
PosixErrorOr<std::vector<int>> Harvest(int pagemap, uintptr_t start) {
  std::vector<PageRegion> vec(kPages);
  PmScanArg arg = {};
  arg.size = sizeof(arg);
  arg.flags = kPmScanWpMatching | kPmScanCheckWpasync;
  arg.start = start;
  arg.end = start + MappingSize();
  arg.vec = reinterpret_cast<uint64_t>(vec.data());
  arg.vec_len = vec.size();
  arg.category_mask = kPageIsWritten;
  arg.return_mask = kPageIsWritten;
  int n = ioctl(pagemap, kPagemapScan, &arg);
  if (n < 0) {
    return PosixError(errno, "PAGEMAP_SCAN");
  }
  std::vector<int> pages;
  for (int i = 0; i < n; i++) {
    for (uint64_t a = vec[i].start; a < vec[i].end; a += kPageSize) {
      pages.push_back(static_cast<int>((a - start) / kPageSize));
    }
  }
  return pages;
}

// kStubTimeoutMillis is how long a stub may take to run a command.
constexpr int kStubTimeoutMillis = 10000;

// A Stub is a child process that shares this process's FD table, as systrap's
// stubs share the Sentry's, and runs commands in its own address space.
class Stub {
 public:
  enum Op : int {
    // Map the memfd shared, writable, and return its address; populate its
    // page tables by writing every page if arg is 1.
    kMap,
    // Create a userfaultfd and return it; without UFFD_USER_MODE_ONLY if arg
    // is 1.
    kUserfaultfd,
    // Store to page arg.
    kStore,
    // read(2) a page from the data pipe into page arg: a write by the kernel.
    kReadInto,
    // Zap the page tables of pages [arg, arg+4) (MADV_DONTNEED).
    kZap,
    // Unmap the mapping and map the memfd again at the same address.
    kRemap,
  };

  Stub() = default;
  Stub(const Stub&) = delete;
  Stub& operator=(const Stub&) = delete;

  ~Stub() {
    if (pid_ > 0) {
      // Closing its command pipe ends the stub.
      to_.reset();
      int status;
      RetryEINTR(waitpid)(pid_, &status, 0);
    }
  }

  // Start starts the stub, in a new user namespace if new_userns, with memfd
  // to map and data to read pages from.
  PosixError Start(int memfd, int data, bool new_userns) {
    int to[2], from[2];
    if (pipe2(to, O_CLOEXEC) < 0) {
      return PosixError(errno, "pipe2");
    }
    in_ = FileDescriptor(to[0]);
    to_ = FileDescriptor(to[1]);
    if (pipe2(from, O_CLOEXEC) < 0) {
      return PosixError(errno, "pipe2");
    }
    from_ = FileDescriptor(from[0]);
    out_ = FileDescriptor(from[1]);
    unsigned long flags = CLONE_FILES | SIGCHLD;
    if (new_userns) {
      flags |= CLONE_NEWUSER;
    }
    // A clone without a new stack continues on a copy of this one, as fork.
    pid_t pid = syscall(SYS_clone, flags, 0, 0, 0, 0);
    if (pid < 0) {
      return PosixError(errno, "clone");
    }
    if (pid == 0) {
      Main(memfd, data, in_.get(), out_.get());
    }
    pid_ = pid;
    return NoError();
  }

  pid_t pid() const { return pid_; }

  // Run makes the stub run op with arg, and returns what it returns, or an
  // error if the operation failed.
  PosixErrorOr<int64_t> Run(Op op, int64_t arg) {
    int64_t cmd[2] = {op, arg};
    if (WriteFd(to_.get(), cmd, sizeof(cmd)) != sizeof(cmd)) {
      return PosixError(errno, "writing a command to the stub");
    }
    // The stub's end of the answer pipe is in the FD table that this process
    // shares, so it does not close if the stub dies: wait for a while only.
    struct pollfd pfd = {};
    pfd.fd = from_.get();
    pfd.events = POLLIN;
    int n;
    do {
      n = poll(&pfd, 1, kStubTimeoutMillis);
    } while (n < 0 && errno == EINTR);
    if (n <= 0) {
      return PosixError(n < 0 ? errno : ETIMEDOUT,
                        absl::StrCat("waiting for stub operation ", op));
    }
    int64_t ret;
    if (ReadFd(from_.get(), &ret, sizeof(ret)) != sizeof(ret)) {
      return PosixError(EPIPE, "reading the stub's answer");
    }
    if (ret < 0) {
      return PosixError(-ret,
                        absl::StrCat("stub operation ", op, "(", arg, ")"));
    }
    return ret;
  }

 private:
  // Main runs the stub's commands until its command pipe is closed. It only
  // makes system calls: this process may be multithreaded, and the stub is a
  // copy of it.
  [[noreturn]] static void Main(int memfd, int data, int in, int out) {
    char* m = nullptr;
    int64_t cmd[2];
    while (read(in, cmd, sizeof(cmd)) == sizeof(cmd)) {
      int64_t arg = cmd[1];
      int64_t ret = 0;
      switch (cmd[0]) {
        case kMap: {
          void* p = mmap(nullptr, MappingSize(), PROT_READ | PROT_WRITE,
                         MAP_SHARED, memfd, 0);
          if (p == MAP_FAILED) {
            ret = -errno;
            break;
          }
          m = static_cast<char*>(p);
          if (arg == 1) {
            for (size_t off = 0; off < MappingSize(); off += kPageSize) {
              m[off] = 's';
            }
          }
          ret = reinterpret_cast<int64_t>(m);
          break;
        }
        case kUserfaultfd: {
          int flags = kUserfaultfdFlags;
          if (arg == 1) {
            flags &= ~UFFD_USER_MODE_ONLY;
          }
          ret = syscall(SYS_userfaultfd, flags);
          if (ret < 0) {
            ret = -errno;
          }
          break;
        }
        case kStore:
          *reinterpret_cast<volatile char*>(m + arg * kPageSize) = 'w';
          break;
        case kReadInto:
          if (read(data, m + arg * kPageSize, kPageSize) !=
              static_cast<ssize_t>(kPageSize)) {
            ret = -EIO;
          }
          break;
        case kZap:
          if (madvise(m + arg * kPageSize, 4 * kPageSize, MADV_DONTNEED) < 0) {
            ret = -errno;
          }
          break;
        case kRemap:
          if (munmap(m, MappingSize()) < 0 ||
              mmap(m, MappingSize(), PROT_READ | PROT_WRITE,
                   MAP_SHARED | MAP_FIXED, memfd, 0) != m) {
            ret = -errno;
          }
          break;
        default:
          ret = -EINVAL;
      }
      if (write(out, &ret, sizeof(ret)) != sizeof(ret)) {
        break;
      }
    }
    _exit(0);
  }

  pid_t pid_ = -1;

  // The command pipe (to the stub) and the answer pipe (from it). The stub
  // shares this process's FD table, so this process keeps the stub's ends
  // open, in_ and out_, until the stub exits.
  FileDescriptor to_;
  FileDescriptor in_;
  FileDescriptor from_;
  FileDescriptor out_;
};

// A TrackedMapping is a mapping of the memfd, in this process or a stub,
// whose writes are tracked.
struct TrackedMapping {
  uintptr_t start = 0;
  FileDescriptor uffd;
  FileDescriptor pagemap;
};

// UffdWPTest maps the memfd in this process ("P") and two stubs, "A", whose
// mapping is populated, and "B", whose mapping is not, and tracks the writes
// through each mapping. Its parameter is true if the stubs run in a new user
// namespace, unprivileged in the initial one, as in runsc's sandbox.
class UffdWPTest : public ::testing::TestWithParam<bool> {
 protected:
  void SetUp() override {
    if (PosixError err = WriteTrackingAvailable(); !err.ok()) {
      const char* require = getenv("GVISOR_REQUIRE_UFFD_WP");
      if (require != nullptr && std::string(require) == "1") {
        FAIL() << "write tracking is not available, and "
                  "GVISOR_REQUIRE_UFFD_WP=1 requires it: "
               << err.ToString();
      }
      GTEST_SKIP() << "write tracking is not available: " << err.ToString();
    }

    int memfd = memfd_create("uffd-wp-test", MFD_CLOEXEC);
    ASSERT_THAT(memfd, SyscallSucceeds());
    memfd_ = FileDescriptor(memfd);
    ASSERT_THAT(ftruncate(memfd_.get(), MappingSize()), SyscallSucceeds());
    int data[2];
    ASSERT_THAT(pipe2(data, O_CLOEXEC), SyscallSucceeds());
    data_read_ = FileDescriptor(data[0]);
    data_write_ = FileDescriptor(data[1]);
    procfs_ = ASSERT_NO_ERRNO_AND_VALUE(
        Open("/proc", O_RDONLY | O_DIRECTORY | O_CLOEXEC));

    // P: this process, whose mapping is populated.
    void* p = mmap(nullptr, MappingSize(), PROT_READ | PROT_WRITE, MAP_SHARED,
                   memfd_.get(), 0);
    ASSERT_NE(p, MAP_FAILED) << strerror(errno);
    p_mapping_ = static_cast<char*>(p);
    memset(p_mapping_, 'p', MappingSize());
    int uffd = syscall(SYS_userfaultfd, kUserfaultfdFlags);
    ASSERT_THAT(uffd, SyscallSucceeds());
    p_.uffd = FileDescriptor(uffd);
    ASSERT_NO_ERRNO(UserfaultfdAPI(p_.uffd.get()));
    p_.start = reinterpret_cast<uintptr_t>(p_mapping_);
    ASSERT_NO_ERRNO(WriteProtect(p_.uffd.get(), p_.start));
    p_.pagemap = ASSERT_NO_ERRNO_AND_VALUE(
        OpenAt(procfs_.get(), "self/pagemap", O_RDONLY | O_CLOEXEC));

    // A and B: stubs, which create the userfaultfds of their address spaces
    // for this process to use.
    if (PosixError err =
            a_stub_.Start(memfd_.get(), data_read_.get(), GetParam());
        !err.ok()) {
      if (GetParam() && err.errno_value() == EPERM) {
        GTEST_SKIP() << "user namespaces are not available: " << err.ToString();
      }
      FAIL() << err.ToString();
    }
    ASSERT_NO_ERRNO(b_stub_.Start(memfd_.get(), data_read_.get(), GetParam()));
    ASSERT_NO_FATAL_FAILURE(TrackStub(&a_stub_, true, &a_));
    ASSERT_NO_FATAL_FAILURE(TrackStub(&b_stub_, false, &b_));
  }

  void TearDown() override {
    if (p_mapping_ != nullptr) {
      munmap(p_mapping_, MappingSize());
    }
  }

  // TrackStub maps the memfd in stub, populated if populate is true, and
  // tracks the writes through that mapping in m.
  void TrackStub(Stub* stub, bool populate, TrackedMapping* m) {
    m->start = ASSERT_NO_ERRNO_AND_VALUE(stub->Run(Stub::kMap, populate));
    int uffd = ASSERT_NO_ERRNO_AND_VALUE(stub->Run(Stub::kUserfaultfd, 0));
    // The stub's userfaultfd is in the FD table this process shares.
    m->uffd = FileDescriptor(uffd);
    ASSERT_NO_ERRNO(UserfaultfdAPI(m->uffd.get()));
    ASSERT_NO_ERRNO(WriteProtect(m->uffd.get(), m->start));
    m->pagemap = ASSERT_NO_ERRNO_AND_VALUE(
        OpenAt(procfs_.get(), absl::StrCat(stub->pid(), "/pagemap"),
               O_RDONLY | O_CLOEXEC));
  }

  // ExpectWritten harvests every mapping, and expects the pages written
  // through P, A and B to be p, a and b.
  void ExpectWritten(const std::string& what, std::vector<int> p,
                     std::vector<int> a, std::vector<int> b) {
    EXPECT_THAT(Harvest(p_.pagemap.get(), p_.start),
                IsPosixErrorOkAndHolds(::testing::ElementsAreArray(p)))
        << "P, " << what;
    EXPECT_THAT(Harvest(a_.pagemap.get(), a_.start),
                IsPosixErrorOkAndHolds(::testing::ElementsAreArray(a)))
        << "A, " << what;
    EXPECT_THAT(Harvest(b_.pagemap.get(), b_.start),
                IsPosixErrorOkAndHolds(::testing::ElementsAreArray(b)))
        << "B, " << what;
  }

  FileDescriptor memfd_;
  FileDescriptor data_read_;
  FileDescriptor data_write_;
  FileDescriptor procfs_;
  char* p_mapping_ = nullptr;
  TrackedMapping p_;
  TrackedMapping a_;
  TrackedMapping b_;
  Stub a_stub_;
  Stub b_stub_;
};

// Writes made before a mapping is write-protected are not reported.
TEST_P(UffdWPTest, NothingWrittenAfterArming) {
  ExpectWritten("after arming", {}, {}, {});
}

// Writes are recorded in the page tables of the address space that made them
// only: a page of the memfd is written if it is reported written through any
// of the mappings.
TEST_P(UffdWPTest, WritesArePerAddressSpace) {
  ASSERT_NO_ERRNO(a_stub_.Run(Stub::kStore, 3));
  ExpectWritten("after A stores to page 3", {}, {3}, {});
  p_mapping_[9 * kPageSize] = 'w';
  ExpectWritten("after P stores to page 9", {9}, {}, {});
}

// Pages that were never faulted in when the mapping was write-protected are
// reported once written (UFFD_FEATURE_WP_UNPOPULATED), as the pages of a new
// stub mapping are.
TEST_P(UffdWPTest, UnpopulatedPages) {
  ASSERT_NO_ERRNO(b_stub_.Run(Stub::kStore, 2));
  ASSERT_NO_ERRNO(b_stub_.Run(Stub::kStore, 30));
  ExpectWritten("after B stores to pages 2 and 30", {}, {}, {2, 30});
}

// Writes made by the kernel on behalf of the process (read(2) into the
// mapping) are reported, although userfaultfds are created with
// UFFD_USER_MODE_ONLY.
TEST_P(UffdWPTest, KernelModeWrites) {
  std::vector<char> page(kPageSize, 'r');
  ASSERT_THAT(WriteFd(data_write_.get(), page.data(), page.size()),
              SyscallSucceedsWithValue(page.size()));
  ASSERT_NO_ERRNO(b_stub_.Run(Stub::kReadInto, 5));
  ASSERT_THAT(WriteFd(data_write_.get(), page.data(), page.size()),
              SyscallSucceedsWithValue(page.size()));
  ASSERT_NO_ERRNO(a_stub_.Run(Stub::kReadInto, 6));
  ExpectWritten("after A and B read(2) into pages 6 and 5", {}, {6}, {5});
}

// Writes through the memfd rather than a mapping are not reported: gVisor
// marks the pages it writes so itself.
TEST_P(UffdWPTest, FileWritesAreNotReported) {
  std::vector<char> page(kPageSize, 'f');
  ASSERT_THAT(pwrite(memfd_.get(), page.data(), page.size(), 7 * kPageSize),
              SyscallSucceedsWithValue(page.size()));
  ExpectWritten("after pwrite(2) to page 7", {}, {}, {});
}

// A harvest write-protects the pages it reports again.
TEST_P(UffdWPTest, HarvestRearms) {
  ASSERT_NO_ERRNO(a_stub_.Run(Stub::kStore, 4));
  ExpectWritten("after A stores to page 4", {}, {4}, {});
  ExpectWritten("after a harvest", {}, {}, {});
  ASSERT_NO_ERRNO(a_stub_.Run(Stub::kStore, 4));
  ExpectWritten("after A stores to page 4 again", {}, {4}, {});
}

// Zapping a mapping's page tables, by MADV_DONTNEED or by punching a hole in
// the memfd (as MemoryFiles decommit pages), loses neither the written state
// of its pages nor their write-protection; zapped pages that were not written
// stay unwritten. This holds with the masks of PAGEMAP_SCAN's fast path on
// kernels without commit 07b4377bdbe7 too.
TEST_P(UffdWPTest, ZapsKeepWrittenState) {
  ASSERT_NO_ERRNO(a_stub_.Run(Stub::kStore, 11));
  ASSERT_NO_ERRNO(a_stub_.Run(Stub::kZap, 10));
  ExpectWritten("after A stores to page 11 and zaps pages 10-13", {}, {11}, {});
  ASSERT_NO_ERRNO(a_stub_.Run(Stub::kStore, 12));
  ExpectWritten("after A stores to zapped page 12", {}, {12}, {});

  ASSERT_NO_ERRNO(a_stub_.Run(Stub::kStore, 20));
  ASSERT_THAT(
      fallocate(memfd_.get(), FALLOC_FL_PUNCH_HOLE | FALLOC_FL_KEEP_SIZE,
                20 * kPageSize, 4 * kPageSize),
      SyscallSucceeds());
  ExpectWritten("after A stores to page 20 and pages 20-23 are punched", {},
                {20}, {});
  ASSERT_NO_ERRNO(a_stub_.Run(Stub::kStore, 22));
  ExpectWritten("after A stores to punched page 22", {}, {22}, {});
}

// Unmapping a mapping loses what was written through it: the new mapping at
// its address is neither registered nor write-protected, and a harvest with
// PM_SCAN_CHECK_WPASYNC fails rather than report it whole. So write tracking
// harvests a stub's mapping before unmapping or replacing it.
TEST_P(UffdWPTest, RemapLosesWrittenState) {
  ASSERT_NO_ERRNO(a_stub_.Run(Stub::kStore, 15));
  ASSERT_NO_ERRNO(a_stub_.Run(Stub::kRemap, 0));
  EXPECT_THAT(Harvest(a_.pagemap.get(), a_.start), PosixErrorIs(EPERM));
  ASSERT_NO_ERRNO(WriteProtect(a_.uffd.get(), a_.start));
  ExpectWritten("after A remaps its mapping and it is write-protected again",
                {}, {}, {});
  ASSERT_NO_ERRNO(a_stub_.Run(Stub::kStore, 17));
  ExpectWritten("after A stores to page 17 of its new mapping", {}, {17}, {});
}

// The fdinfo of a pidfd shows the process's PID in the procfs's PID
// namespace, which is how the Sentry finds a stub's PID in a procfs of an
// ancestor PID namespace.
TEST_P(UffdWPTest, PidfdFdinfoShowsPID) {
  int pidfd = syscall(SYS_pidfd_open, a_stub_.pid(), 0);
  ASSERT_THAT(pidfd, SyscallSucceeds());
  FileDescriptor fd(pidfd);
  FileDescriptor fdinfo = ASSERT_NO_ERRNO_AND_VALUE(
      OpenAt(procfs_.get(), absl::StrCat("self/fdinfo/", pidfd), O_RDONLY));
  std::vector<char> buf(4096);
  ssize_t n = ReadFd(fdinfo.get(), buf.data(), buf.size());
  ASSERT_THAT(n, SyscallSucceeds());
  std::vector<std::string> lines =
      absl::StrSplit(std::string(buf.data(), n), '\n');
  EXPECT_THAT(lines,
              ::testing::Contains(absl::StrCat("Pid:\t", a_stub_.pid())));
}

// A process without CAP_SYS_PTRACE in the initial user namespace, such as a
// stub in a new user namespace, can create a userfaultfd where
// vm.unprivileged_userfaultfd is 0 only with UFFD_USER_MODE_ONLY, which is why
// write tracking's userfaultfds have it.
TEST_P(UffdWPTest, UnprivilegedNeedsUserModeOnly) {
  if (!GetParam()) {
    GTEST_SKIP() << "the stubs may be privileged";
  }
  FileDescriptor sysctl = ASSERT_NO_ERRNO_AND_VALUE(
      Open("/proc/sys/vm/unprivileged_userfaultfd", O_RDONLY));
  char c;
  ASSERT_THAT(ReadFd(sysctl.get(), &c, 1), SyscallSucceedsWithValue(1));
  if (c != '0') {
    GTEST_SKIP() << "vm.unprivileged_userfaultfd is not 0";
  }
  EXPECT_THAT(b_stub_.Run(Stub::kUserfaultfd, 1), PosixErrorIs(EPERM));
}

INSTANTIATE_TEST_SUITE_P(Stubs, UffdWPTest, ::testing::Bool(),
                         [](const ::testing::TestParamInfo<bool>& info) {
                           return info.param ? "NewUserNamespace"
                                             : "SameUserNamespace";
                         });

}  // namespace

}  // namespace testing
}  // namespace gvisor
