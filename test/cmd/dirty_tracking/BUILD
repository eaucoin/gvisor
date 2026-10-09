load("//tools:defs.bzl", "cc_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

cc_binary(
    name = "escapes",
    testonly = 1,
    srcs = ["escapes.c"],
    linkstatic = 1,
    visibility = ["//runsc/container:__pkg__"],
)

cc_binary(
    name = "mem_touch",
    testonly = 1,
    srcs = ["mem_touch.c"],
    linkstatic = 1,
    visibility = ["//runsc/container:__pkg__"],
)
