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

ARG DROID_VERSION=0.238.0
# From https://downloads.factory.ai/factory-cli/releases/${DROID_VERSION}/linux/<arch>/droid.sha256
#
# amd64 uses the vendor's "-baseline" build: the installer picks between it and an AVX2 build
# by reading /proc/cpuinfo, which a Dockerfile cannot do for a guest that does not exist yet.
ARG DROID_SHA256_AMD64=a6451f315d210607ef30b0230a87e50687c26aa1b00826864ac4416c5e3dd75d
ARG DROID_SHA256_ARM64=b62bbc7caff53dc9e17a93f9d670c6e9b04ef348155c8f6f96e6c97024e2d1fd

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
