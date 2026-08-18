# Google Gemini CLI.
#
# Channel: vendor-published release bundle, pinned and checksummed.
#
# Gemini CLI publishes no Linux native binary — the only Linux artifact is
# `gemini-cli-bundle.zip`, a self-contained JavaScript bundle with its dependencies already
# inlined. That is still one pinned, checksummed file and zero packages installed at build
# time, so it beats both npm and pnpm here; the only thing it needs from the base image is
# Node, which is already there.
#
# The bundle is single-digest per release rather than per-architecture, because it is
# JavaScript: the same bytes run on amd64 and arm64.
ARG BASE_IMAGE=ghcr.io/dagsommer/boks/base:0.1.0
FROM ${BASE_IMAGE}

USER root

ARG GEMINI_CLI_VERSION=0.54.4
ARG GEMINI_CLI_SHA256=974cd0db9d9b40be46858e638c8e017775b5f4741626822b0fce3d3c49216515

RUN set -eux; \
    curl -fsSL -o /tmp/gemini-cli-bundle.zip \
      "https://github.com/google-gemini/gemini-cli/releases/download/v${GEMINI_CLI_VERSION}/gemini-cli-bundle.zip"; \
    echo "${GEMINI_CLI_SHA256}  /tmp/gemini-cli-bundle.zip" | sha256sum -c -; \
    mkdir -p /opt/gemini-cli; \
    unzip -q /tmp/gemini-cli-bundle.zip -d /opt/gemini-cli; \
    rm -f /tmp/gemini-cli-bundle.zip; \
    chmod 0755 /opt/gemini-cli/gemini.js; \
    # The bundle entrypoint carries a `#!/usr/bin/env node` shebang, so a symlink is enough
    # — no wrapper script to keep in step with it.
    ln -s /opt/gemini-cli/gemini.js /usr/local/bin/gemini; \
    gemini --version

# Numeric on purpose: `USER agent` silently means uid 0 off Linux. The full reason is
# in images/base/Dockerfile, above its own USER line.
USER 1000:1000
CMD ["gemini"]
