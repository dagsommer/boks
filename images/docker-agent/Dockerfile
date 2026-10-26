# Docker Agent — Docker Engineering's agent builder and runtime, formerly `cagent`.
#
# A single static Go binary from the project's GitHub releases, pinned and checksummed. No
# install script and no dependency tree at all, which makes this the cheapest agent here in
# both size and supply-chain surface.
ARG BASE_IMAGE=ghcr.io/dagsommer/boks/base:0.1.0
FROM ${BASE_IMAGE}

USER root

ARG DOCKER_AGENT_VERSION=1.152.0
ARG DOCKER_AGENT_SHA256_AMD64=bb591b3b73e6ce06b24a19732371fefb5a99f001813c94e146a4b1474135c8e8
ARG DOCKER_AGENT_SHA256_ARM64=a048a1b5da4d57b1e9669e72460e16153ea7185722b74a411ea0a9b5341ec94f

RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      amd64) sum="${DOCKER_AGENT_SHA256_AMD64}" ;; \
      arm64) sum="${DOCKER_AGENT_SHA256_ARM64}" ;; \
      *) echo "boks: unsupported architecture $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/docker-agent \
      "https://github.com/docker/docker-agent/releases/download/v${DOCKER_AGENT_VERSION}/docker-agent-linux-${arch}"; \
    echo "${sum}  /tmp/docker-agent" | sha256sum -c -; \
    install -m 0755 /tmp/docker-agent /usr/local/bin/docker-agent; \
    rm -f /tmp/docker-agent; \
    docker-agent version

# Numeric on purpose: `USER agent` silently means uid 0 off Linux. The full reason is
# in images/base/Dockerfile, above its own USER line.
USER 1000:1000

# Docker Agent collects anonymous usage data unless told not to. Boks reports no telemetry of
# its own, and an image that quietly phones home from inside a sandbox would make that claim
# false in practice, so the opt-out is the image's default. A user who wants it can set the
# variable back with `boks run -e TELEMETRY_ENABLED=true`.
ENV TELEMETRY_ENABLED=false

CMD ["docker-agent"]
