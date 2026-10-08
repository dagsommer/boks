# The shared pi agent image.
#
# Installs @earendil-works/pi-coding-agent from npm, pinned and checksummed.
#
# Unlike the other agent images which download vendor-published binaries, pi is an
# npm package — but the same supply-chain discipline applies: we do not run arbitrary
# code from the build machine during `docker build`. The dependency tree is small
# enough (about 25 packages) that a postinstall compromise would still be a
# compromise of the build host, beside whatever credential publishes to GHCR.
#
# We download the tarball, verify its SRI integrity (sha512) from npm registry
# metadata, then install it with `npm install` with `--ignore-scripts` to skip
# lifecycle hooks entirely. This is the same approach the other images use:
# vendor artifact, pinned version, verified checksum, zero build-time scripts.
#
# 1.0.4, not the 0.74.0 this image first shipped: 0.74.0 has no llama.cpp support, and the
# built-in llama.cpp provider (/llama to load, unload and download models on a router, /model
# for what is loaded) is what pi-local is for. See boks-pi-local.

ARG BASE_IMAGE=ghcr.io/dagsommer/boks/base:0.1.0
FROM ${BASE_IMAGE}

USER root

ARG PI_VERSION=1.1.0
# From https://registry.npmjs.org/@earendil-works/pi-coding-agent/${PI_VERSION}
# The npm registry provides a SHA-1 shasum (called "shasum"); we verify with sha512
# from the SRI integrity field instead.
ARG PI_INTEGRITY=sha512-SeEi/4hdcHNgA9UWlefZl7ZZpm3dzi2OoxNjDHsBJ9o298LNOtbL4DGKgitlEj6uCTccvtw6f2hlCkTPVJ2RXg==

RUN set -eux; \
    # Download the npm tarball.
    curl -fsSL -o /tmp/pi.tgz "https://registry.npmjs.org/@earendil-works/pi-coding-agent/-/pi-coding-agent-${PI_VERSION}.tgz"; \
    # Verify integrity (sha512 SRI hash from npm registry metadata).
    expected="${PI_INTEGRITY#sha512-}"; \
    actual="$(openssl dgst -sha512 -binary < /tmp/pi.tgz | base64 -w 0)"; \
    test "$expected" = "$actual"; \
    # Install globally with --ignore-scripts to skip lifecycle hooks (postinstall etc).
    # -g installs to NPM_CONFIG_PREFIX (defaults to /usr/local for root).
    # --omit=dev ensures devDependencies are not installed.
    npm install -g --ignore-scripts --omit=dev /tmp/pi.tgz; \
    rm -f /tmp/pi.tgz; \
    # Installed as root into the agent's own npm prefix, which left root-owned directories
    # in it and a root-owned ~/.npm cache: the agent could not update pi or install any
    # global package ("The operation was rejected by your operating system"). Hand both back.
    chown -R 1000:1000 "$(npm config get prefix)" /home/agent/.npm 2>/dev/null || true; \
    pi --version

# pi extensions, from the pinned lockfile in extensions/: every package in it, dependencies
# included, carries a sha512 integrity that `npm ci` checks, and --ignore-scripts keeps their
# lifecycle hooks from running, as for pi itself. They live under /opt, owned by root, so they
# belong to the image rather than to a sandbox's home: a new image version replaces them, and
# the agent cannot rewrite what pi loads. prepare.d/20-pi-extensions.sh registers them.
# --legacy-peer-deps because each names pi, pi-tui and typebox as peers, which pi provides to
# its extensions at runtime; installing them would only add a second, unused copy of pi.
COPY extensions/package.json extensions/package-lock.json /opt/pi-extensions/
RUN set -eux; \
    cd /opt/pi-extensions; \
    npm ci --ignore-scripts --legacy-peer-deps --omit=dev; \
    npm cache clean --force

# Prepare.d scripts run at sandbox start-up via boks-prepare.
# This one discovers model servers on opened host ports and writes models.json.
COPY --chmod=0755 prepare.d/ /etc/boks/prepare.d/
# Exports the llama.cpp router the prepare script found, then becomes pi. See the file.
COPY --chmod=0755 boks-pi-local /usr/local/bin/boks-pi-local

USER 1000:1000
WORKDIR /home/agent

CMD ["pi"]
