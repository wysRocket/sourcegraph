load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_file", "http_archive")

DOCSITE_VERSION = "1.9.3"
SRC_CLI_VERSION = "5.1.0"

SRC_CLI_BUILDFILE = """
filegroup(
    name = "src-cli-{}",
    srcs = ["src"],
    visibility = ["//visibility:public"],
)
"""

def tool_deps():
    # Docsite #
    http_file(
        name = "docsite_darwin_amd64",
        urls = ["https://github.com/sourcegraph/docsite/releases/download/v{0}/docsite_v{0}_darwin_amd64".format(DOCSITE_VERSION)],
        sha256 = "1bd76fba15dd6e11fc5215c7307eba38cf7db75cd739613e301c899c8cdf2c1b",
        executable = True,
    )

    http_file(
        name = "docsite_darwin_arm64",
        urls = ["https://github.com/sourcegraph/docsite/releases/download/v{0}/docsite_v{0}_darwin_arm64".format(DOCSITE_VERSION)],
        sha256 = "a494aad157dfc0b2de3b5126908c31fda64220ac5b59d1ec91ad4dfd4e1e1343",
        executable = True,
    )

    http_file(
        name = "docsite_linux_amd64",
        urls = ["https://github.com/sourcegraph/docsite/releases/download/v{0}/docsite_v{0}_linux_amd64".format(DOCSITE_VERSION)],
        sha256 = "99f3b38b7590c2e9d1b94bde0352535210ee8a328e055d5a0d51d09ef5849f07",
        executable = True,
    )

    # src-cli #
    http_archive(
        name = "src-cli-linux-amd64",
        build_file_content = SRC_CLI_BUILDFILE.format("linux-amd64"),
        sha256 = "270ddad7748c1b76f082b637e336b5c7a58af76d207168469f4b7bef957953e3",
        url = "https://github.com/sourcegraph/src-cli/releases/download/{0}/src-cli_{0}_linux_amd64.tar.gz".format(SRC_CLI_VERSION),
    )

    http_archive(
        name = "src-cli-darwin-amd64",
        build_file_content = SRC_CLI_BUILDFILE.format("darwin-amd64"),
        sha256 = "f14414e3ff4759cd1fbed0107138214f87d9a69cdb55ed1c4522704069420d9b",
        url = "https://github.com/sourcegraph/src-cli/releases/download/{0}/src-cli_{0}_darwin_amd64.tar.gz".format(SRC_CLI_VERSION),
    )

    http_archive(
        name = "src-cli-darwin-arm64",
        build_file_content = SRC_CLI_BUILDFILE.format("darwin-arm64"),
        sha256 = "93dc6c8522792ea16e3c8c81c8cf655a908118e867fda43c048c9b51f4c70e88",
        url = "https://github.com/sourcegraph/src-cli/releases/download/{0}/src-cli_{0}_darwin_arm64.tar.gz".format(SRC_CLI_VERSION),
    )

