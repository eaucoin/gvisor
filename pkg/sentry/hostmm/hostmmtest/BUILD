load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "hostmmtest",
    testonly = 1,
    srcs = ["hostmmtest.go"],
    visibility = ["//pkg/sentry:internal"],
)
