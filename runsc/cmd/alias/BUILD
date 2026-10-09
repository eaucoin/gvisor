load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "alias",
    srcs = ["aliases.go"],
    visibility = ["//runsc:__subpackages__"],
    deps = [
        "//runsc/cmd/alias/bwrap",
        "//runsc/cmd/util",
    ],
)

go_test(
    name = "alias_test",
    size = "small",
    srcs = ["aliases_test.go"],
    library = ":alias",
)
