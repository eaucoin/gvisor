load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "s3",
    srcs = [
        "errors.go",
        "reader.go",
        "s3.go",
        "writer.go",
    ],
    visibility = [
        "//runsc:__subpackages__",
    ],
    deps = [
        "//pkg/log",
        "//pkg/sentry/memmap",
        "//pkg/sentry/state/checkpointfiles",
        "//pkg/sentry/state/checkpointimage",
        "//pkg/sentry/state/stateio",
        "//pkg/sync",
        "@com_github_aws_aws_sdk_go_v2//aws:go_default_library",
        "@com_github_aws_aws_sdk_go_v2//aws/ratelimit:go_default_library",
        "@com_github_aws_aws_sdk_go_v2//aws/retry:go_default_library",
        "@com_github_aws_aws_sdk_go_v2//aws/transport/http:go_default_library",
        "@com_github_aws_aws_sdk_go_v2_config//:go_default_library",
        "@com_github_aws_aws_sdk_go_v2_service_s3//:go_default_library",
        "@com_github_aws_aws_sdk_go_v2_service_s3//types:go_default_library",
        "@com_github_aws_smithy_go//:go_default_library",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "s3_test",
    size = "small",
    srcs = ["s3_test.go"],
    library = ":s3",
    deps = [
        "//pkg/sentry/memmap",
        "//pkg/sentry/state/checkpointfiles",
        "//pkg/sentry/state/checkpointimage",
        "//pkg/sentry/state/stateio",
        "//runsc/checkpointgofer/s3/s3test",
        "@com_github_aws_aws_sdk_go_v2_credentials//:go_default_library",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
