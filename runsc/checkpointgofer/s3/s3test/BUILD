load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "s3test",
    testonly = 1,
    srcs = ["s3test.go"],
    visibility = [
        "//runsc:__subpackages__",
    ],
    deps = ["//pkg/sync"],
)
