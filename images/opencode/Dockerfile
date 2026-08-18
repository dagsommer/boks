# OpenCode.
#
# Installed from the vendor's release tarball rather than npm. The npm package
# (`opencode-ai`) is a shim whose postinstall downloads the same binary, so `--ignore-scripts`
# leaves it non-functional; taking the artifact directly means one pinned file with a
# checksum recorded here instead of an install script we would have to allow to run.
ARG BASE_IMAGE=ghcr.io/dagsommer/boks/base:0.1.0
FROM ${BASE_IMAGE}

USER root

ARG OPENCODE_VERSION=1.18.16
# sha256 of the release tarballs, recorded at the version above. A bump changes these lines,
# which is the point: the diff is what a reviewer checks.
#
# The amd64 asset is the "baseline" build. The vendor also publishes an AVX2 one, but a
# Dockerfile cannot know what the guest CPU will be — the baseline binary runs on any x86-64.
ARG OPENCODE_SHA256_AMD64=b075251f373b45fa01cc41b7b1fd6eb2b61c9544383e3399a5e7f1447d1fb95f
ARG OPENCODE_SHA256_ARM64=4fdce5f9bc877d977304d71c0c90ad6e83efa381fe0edf0a61e6142a625e1c41

RUN set -eux; \
    case "$(dpkg --print-architecture)" in \
      amd64) asset="opencode-linux-x64-baseline.tar.gz"; sum="${OPENCODE_SHA256_AMD64}" ;; \
      arm64) asset="opencode-linux-arm64.tar.gz";        sum="${OPENCODE_SHA256_ARM64}" ;; \
      *) echo "boks: unsupported architecture $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/opencode.tar.gz \
      "https://github.com/sst/opencode/releases/download/v${OPENCODE_VERSION}/${asset}"; \
    echo "${sum}  /tmp/opencode.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/opencode.tar.gz -C /usr/local/bin opencode; \
    rm -f /tmp/opencode.tar.gz; \
    chmod 0755 /usr/local/bin/opencode; \
    opencode --version

# Numeric on purpose: `USER agent` silently means uid 0 off Linux. The full reason is
# in images/base/Dockerfile, above its own USER line.
USER 1000:1000
CMD ["opencode"]
