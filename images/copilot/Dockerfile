# GitHub Copilot CLI.
#
# Channel: vendor-published native binary from the project's GitHub release, pinned and
# checksummed. Not npm — `@github/copilot` wraps this same binary behind a package install
# (11 packages beyond the base image's own npm) that buys nothing once the artifact is taken
# directly.
ARG BASE_IMAGE=ghcr.io/dagsommer/boks/base:0.1.0
FROM ${BASE_IMAGE}

USER root

ARG COPILOT_VERSION=1.0.95
ARG COPILOT_SHA256_AMD64=c91a8874f3b2bdc905c2452f9d9fc923013924feb431e256eb769ff792719d7f
ARG COPILOT_SHA256_ARM64=4cae94d3abd15699d6a479103a9a39fb3c1dcd85a6ce191882f1902340f10f44

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
