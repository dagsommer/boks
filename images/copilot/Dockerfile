# GitHub Copilot CLI.
#
# Channel: vendor-published native binary from the project's GitHub release, pinned and
# checksummed. Not npm — `@github/copilot` wraps this same binary behind a package install
# (11 packages beyond the base image's own npm) that buys nothing once the artifact is taken
# directly.
ARG BASE_IMAGE=ghcr.io/dagsommer/boks/base:0.1.0
FROM ${BASE_IMAGE}

USER root

ARG COPILOT_VERSION=1.0.114
ARG COPILOT_SHA256_AMD64=b91a96f2257297cfb67ed6784a2a658609a6b8c2ee2ded18af9c9abe8cc790ae
ARG COPILOT_SHA256_ARM64=a204154d4522a7c3aa73a65ba598e432f6aafe37d5fa31cf853163a27d98018e

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
