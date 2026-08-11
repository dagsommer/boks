# Claude Code.
#
# Channel: vendor-published native binary, pinned and checksummed. Not npm.
#
# `@anthropic-ai/claude-code` is a wrapper whose install script links a native binary out of
# an optional dependency: with `--ignore-scripts` it does not work at all ("claude native
# binary not installed"), and without it every package in the tree gets to run postinstall on
# the build machine. The tree is small — measured at 2 packages beyond the base image's own
# npm — but the exposure is not the count, it is that arbitrary code runs next to whatever
# credential the build has. Anthropic publishes the same native binary directly, with a
# per-platform SHA-256 in a manifest, so this takes that instead: one artifact, one checksum,
# no install script and no package manager.
#
# https://claude.ai/install.sh is the documented route to the same files. It is not used
# here: piping an unpinned script from a URL into bash is the thing we are avoiding. This
# Dockerfile does what that script does, with the version and the digests fixed in the diff.
ARG BASE_IMAGE=ghcr.io/dagsommer/boks/base:0.1.0
FROM ${BASE_IMAGE}

USER root

ARG CLAUDE_CODE_VERSION=2.1.228
# From https://downloads.claude.ai/claude-code-releases/${CLAUDE_CODE_VERSION}/manifest.json,
# the same digests the vendor's installer verifies against. glibc builds, because the base
# image is Debian.
ARG CLAUDE_CODE_SHA256_AMD64=d535985e6941a3eb00179ccd7f52ceb0c6623a0305a518ebc4e6514f84a94c99
ARG CLAUDE_CODE_SHA256_ARM64=2664006219497bf7021ac43156519cd42eda64ceb2a66f434ecab83e7831f942

RUN set -eux; \
    case "$(dpkg --print-architecture)" in \
      amd64) platform=linux-x64;   sum="${CLAUDE_CODE_SHA256_AMD64}" ;; \
      arm64) platform=linux-arm64; sum="${CLAUDE_CODE_SHA256_ARM64}" ;; \
      *) echo "boks: unsupported architecture $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/claude \
      "https://downloads.claude.ai/claude-code-releases/${CLAUDE_CODE_VERSION}/${platform}/claude"; \
    echo "${sum}  /tmp/claude" | sha256sum -c -; \
    install -m 0755 /tmp/claude /usr/local/bin/claude; \
    rm -f /tmp/claude; \
    claude --version

USER agent
CMD ["claude"]
