# Factory Droid.
#
# Factory documents `curl -fsSL https://app.factory.ai/cli | sh`. As with Cursor, the script
# is not used: it is unpinned code from a URL. What it does is download one binary and check
# it against a `.sha256` published beside it, so this Dockerfile downloads the same pinned
# binary and checks it against that same published checksum — recorded here, so a change of
# artifact shows up as a diff rather than as a silently different image.
ARG BASE_IMAGE=ghcr.io/dagsommer/boks/base:0.1.0
FROM ${BASE_IMAGE}

USER root

ARG DROID_VERSION=0.193.0
# From https://downloads.factory.ai/factory-cli/releases/${DROID_VERSION}/linux/<arch>/droid.sha256
#
# amd64 uses the vendor's "-baseline" build: the installer picks between it and an AVX2 build
# by reading /proc/cpuinfo, which a Dockerfile cannot do for a guest that does not exist yet.
ARG DROID_SHA256_AMD64=2794f4e34aeffbc23ae586f0879fe0b4de9f58f92aacc939d348f3140177257d
ARG DROID_SHA256_ARM64=f24a2dac9ae22d92a6c960d903fd4f5fb03839d622bebdaa4f8965f65e2a7555

RUN set -eux; \
    case "$(dpkg --print-architecture)" in \
      amd64) droid_arch=x64-baseline; sum="${DROID_SHA256_AMD64}" ;; \
      arm64) droid_arch=arm64;        sum="${DROID_SHA256_ARM64}" ;; \
      *) echo "boks: unsupported architecture $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/droid \
      "https://downloads.factory.ai/factory-cli/releases/${DROID_VERSION}/linux/${droid_arch}/droid"; \
    echo "${sum}  /tmp/droid" | sha256sum -c -; \
    install -m 0755 /tmp/droid /usr/local/bin/droid; \
    rm -f /tmp/droid; \
    droid --version

# Numeric on purpose: `USER agent` silently means uid 0 off Linux. The full reason is
# in images/base/Dockerfile, above its own USER line.
USER 1000:1000
CMD ["droid"]
