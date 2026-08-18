# Cursor CLI (the `cursor-agent` / `agent` binary).
#
# Cursor documents `curl https://cursor.com/install | bash`. That script is not used here:
# it is unpinned code fetched at build time, which is exactly the shape we are trying to
# avoid. What the script does is resolve a version and download one tarball per platform, so
# this Dockerfile does the same thing with the version pinned and the checksums recorded.
#
# The cost of that choice is honest: the version below is whatever cursor.com/install
# resolved to when this file was written, and there is no vendor-published checksum to
# compare against, so these sums attest "the same bytes we reviewed", not "the bytes Cursor
# signed". That is still strictly better than running the installer.
ARG BASE_IMAGE=ghcr.io/dagsommer/boks/base:0.1.0
FROM ${BASE_IMAGE}

USER root

ARG CURSOR_AGENT_VERSION=2026.08.04-aaa8809
ARG CURSOR_AGENT_SHA256_AMD64=e282068dcb5cdd668b8ce2e3456c58be13bb64a834e1ad49f8534b5cd7aa2fe5
ARG CURSOR_AGENT_SHA256_ARM64=d5196289292a660b59807ac508c9ac36ec1e1a1a7e4697af3ef6824fdea984ee

RUN set -eux; \
    case "$(dpkg --print-architecture)" in \
      amd64) cursor_arch=x64;   sum="${CURSOR_AGENT_SHA256_AMD64}" ;; \
      arm64) cursor_arch=arm64; sum="${CURSOR_AGENT_SHA256_ARM64}" ;; \
      *) echo "boks: unsupported architecture $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/cursor-agent.tar.gz \
      "https://downloads.cursor.com/lab/${CURSOR_AGENT_VERSION}/linux/${cursor_arch}/agent-cli-package.tar.gz"; \
    echo "${sum}  /tmp/cursor-agent.tar.gz" | sha256sum -c -; \
    mkdir -p /opt/cursor-agent; \
    tar -xzf /tmp/cursor-agent.tar.gz -C /opt/cursor-agent --strip-components=1 --no-same-owner; \
    rm -f /tmp/cursor-agent.tar.gz; \
    chmod 0755 /opt/cursor-agent/cursor-agent; \
    # Both spellings, as the vendor installer creates: `agent` is what its own output tells
    # people to type, `cursor-agent` is the older name still in circulation.
    ln -s /opt/cursor-agent/cursor-agent /usr/local/bin/cursor-agent; \
    ln -s /opt/cursor-agent/cursor-agent /usr/local/bin/agent; \
    cursor-agent --version

# Numeric on purpose: `USER agent` silently means uid 0 off Linux. The full reason is
# in images/base/Dockerfile, above its own USER line.
USER 1000:1000
CMD ["cursor-agent"]
