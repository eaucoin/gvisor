load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "image",
    srcs = [
        "files.go",
        "image.go",
        "inspect.go",
        "layers.go",
        "rewrite.go",
        "verify.go",
    ],
    visibility = ["//runsc:__subpackages__"],
    deps = [
        "//pkg/cpuid",
        "//pkg/sentry/platform",
        "//pkg/sentry/state/checkpointfiles",
        "//pkg/sentry/state/checkpointimage",
        "//pkg/state/statefile",
        "//runsc/boot",
        "//runsc/cmd/util",
        "//runsc/config",
        "//runsc/flag",
        "//runsc/version",
        "@com_github_google_subcommands//:go_default_library",
        "@com_github_opencontainers_runtime_spec//specs-go:go_default_library",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "image_test",
    size = "small",
    srcs = ["image_test.go"],
    data = glob(["testdata/**"]),
    library = ":image",
    deps = [
        "//pkg/sentry/pgalloc:pgalloc_metadata_go_proto",
        "//pkg/sentry/state/checkpointfiles",
        "//pkg/sentry/state/checkpointimage",
        "//runsc/boot",
        "//runsc/config",
        "//runsc/version",
        "@com_github_cespare_xxhash_v2//:go_default_library",
        "@com_github_google_subcommands//:go_default_library",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
