# GitHub Copilot CLI.
#
# Channel: vendor-published native binary from the project's GitHub release, pinned and
# checksummed. Not npm — `@github/copilot` wraps this same binary behind a package install
# (11 packages beyond the base image's own npm) that buys nothing once the artifact is taken
# directly.
ARG BASE_IMAGE=ghcr.io/dagsommer/boks/base:0.1.0
FROM ${BASE_IMAGE}

USER root

ARG COPILOT_VERSION=1.0.93
ARG COPILOT_SHA256_AMD64=23208bc3534d157031a3db3421e239169b2a67b98655862c0e1daa6d91afcea2
ARG COPILOT_SHA256_ARM64=d01fd18ba2489e7878d9ddcf46de9817a1a2f803b474c0283e2cfd8f837b5d22

RUN set -eux; \
    case "$(dpkg --print-architecture)" in \
      amd64) copilot_arch=x64;   sum="${COPILOT_SHA256_AMD64}" ;; \
      arm64) copilot_arch=arm64; sum="${COPILOT_SHA256_ARM64}" ;; \
      *) echo "boks: unsupported architecture $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/copilot.tar.gz \
      "https://github.com/github/copilot-cli/releases/download/v${COPILOT_VERSION}/copilot-linux-${copilot_arch}.tar.gz"; \
    echo "${sum}  /tmp/copilot.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/copilot.tar.gz -C /tmp copilot; \
    install -m 0755 /tmp/copilot /usr/local/bin/copilot; \
    rm -f /tmp/copilot.tar.gz /tmp/copilot; \
    copilot --version

# Numeric on purpose: `USER agent` silently means uid 0 off Linux. The full reason is
# in images/base/Dockerfile, above its own USER line.
USER 1000:1000
CMD ["copilot"]
