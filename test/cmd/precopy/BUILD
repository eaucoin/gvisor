load("//tools:defs.bzl", "cc_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

cc_binary(
    name = "abwriter",
    testonly = 1,
    srcs = ["abwriter.c"],
    linkstatic = 1,
    visibility = ["//runsc/container:__pkg__"],
)
