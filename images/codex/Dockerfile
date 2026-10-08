# OpenAI Codex CLI.
#
# Channel: vendor-published native binary from the project's GitHub release, pinned and
# checksummed. Not npm — `@openai/codex` only wraps this same Rust binary, so taking the
# release artifact removes the dependency tree without losing anything.
#
# The Linux artifacts are statically linked against musl, which is why a musl build is used
# on a glibc base: it has no dynamic libc dependency to satisfy.
ARG BASE_IMAGE=ghcr.io/dagsommer/boks/base:0.1.0
FROM ${BASE_IMAGE}

USER root

ARG CODEX_VERSION=0.161.0
ARG CODEX_SHA256_AMD64=b1efb95097660d7f2e5a3887618a23f2ea1b0d548078bf92b0f7a5d229a0cef2
ARG CODEX_SHA256_ARM64=5c90bf4be3c97fc62fcafb28ec2335ee318e6ecb332e1e8ee3d2c63814a5a26a

RUN set -eux; \
    case "$(dpkg --print-architecture)" in \
      amd64) target=x86_64-unknown-linux-musl;  sum="${CODEX_SHA256_AMD64}" ;; \
      arm64) target=aarch64-unknown-linux-musl; sum="${CODEX_SHA256_ARM64}" ;; \
      *) echo "boks: unsupported architecture $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/codex.tar.gz \
      "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/codex-${target}.tar.gz"; \
    echo "${sum}  /tmp/codex.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/codex.tar.gz -C /tmp "codex-${target}"; \
    install -m 0755 "/tmp/codex-${target}" /usr/local/bin/codex; \
    rm -f /tmp/codex.tar.gz "/tmp/codex-${target}"; \
    codex --version

# Numeric on purpose: `USER agent` silently means uid 0 off Linux. The full reason is
# in images/base/Dockerfile, above its own USER line.
USER 1000:1000
CMD ["codex"]
