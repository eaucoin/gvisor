load("//tools:defs.bzl", "cc_test", "select_gtest")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

# Tests of the host kernel's behaviour that the Sentry's write tracking
# (runsc --dirty-tracking=uffd) relies on. They run on the host, not in gVisor.
cc_test(
    name = "uffd_wp_test",
    size = "small",
    srcs = ["uffd_wp_test.cc"],
    linkstatic = 1,
    deps = select_gtest() + [
        "//test/util:file_descriptor",
        "//test/util:posix_error",
        "//test/util:test_main",
        "//test/util:test_util",
        "@com_google_absl//absl/strings",
    ],
)
