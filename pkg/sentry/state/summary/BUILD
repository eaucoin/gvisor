load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "summary",
    srcs = ["summary.go"],
    visibility = ["//:sandbox"],
)

go_test(
    name = "summary_test",
    size = "small",
    srcs = ["summary_test.go"],
    library = ":summary",
)
