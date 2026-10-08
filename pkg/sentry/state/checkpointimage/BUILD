load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "checkpointimage",
    srcs = [
        "checkpointimage.go",
        "image.go",
        "layers.go",
        "metadata.go",
    ],
    visibility = ["//:sandbox"],
    deps = [
        "//pkg/log",
        "//pkg/sentry/pgalloc:pgalloc_metadata_go_proto",
        "//pkg/sentry/state/checkpointfiles",
        "@org_golang_google_protobuf//proto:go_default_library",
    ],
)

go_test(
    name = "checkpointimage_test",
    size = "small",
    srcs = [
        "bench_test.go",
        "checkpointimage_test.go",
        "fuzz_test.go",
    ],
    library = ":checkpointimage",
    deps = [
        "//pkg/sentry/pgalloc:pgalloc_metadata_go_proto",
        "//pkg/sentry/state/checkpointfiles",
        "@org_golang_google_protobuf//proto:go_default_library",
    ],
)
