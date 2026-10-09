load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "hostmm",
    srcs = [
        "cgroup.go",
        "hostmm.go",
        "membarrier.go",
        "uffdwp.go",
        "uffdwp_unsafe.go",
    ],
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/eventfd",
        "//pkg/log",
        "//pkg/seccomp",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "hostmm_test",
    size = "small",
    srcs = ["uffdwp_test.go"],
    library = ":hostmm",
    deps = [
        "//pkg/sentry/hostmm/hostmmtest",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
