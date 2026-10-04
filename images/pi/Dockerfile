# The shared pi agent image.
#
# Installs @earendil-works/pi-coding-agent from npm, pinned and checksummed.
#
# Unlike the other agent images which download vendor-published binaries, pi is an
# npm package — but the same supply-chain discipline applies: we do not run arbitrary
# code from the build machine during `docker build`, and the dependency tree is
# small enough (140 packages) that a postinstall compromise would still be a
# compromise of the build host, beside whatever credential publishes to GHCR.
#
# The alternative is to download the tarball, verify its checksum, extract it, and
# ship the extracted files. That is what this Dockerfile does: it pins the exact
# package version, verifies the shasum from the npm registry metadata, extracts
# only the distributed files, and removes the node_modules tree (pi bundles its
# own dependencies in the dist/ directory and does not need them at runtime).
#
# This avoids running *any* package lifecycle scripts on the build host.

ARG BASE_IMAGE=ghcr.io/dagsommer/boks/base:0.1.0
FROM ${BASE_IMAGE}

USER root

ARG PI_VERSION=0.74.0
# From https://registry.npmjs.org/@earendil-works/pi-coding-agent/${PI_VERSION}
ARG PI_SHA256=4051f28790f8a59d2a05d6bf8192e4ee11124220

RUN set -eux; \
    curl -fsSL -o /tmp/pi.tgz "https://registry.npmjs.org/@earendil-works/pi-coding-agent/-/pi-coding-agent-${PI_VERSION}.tgz"; \
    echo "${PI_SHA256}  /tmp/pi.tgz" | sha256sum -c -; \
    # Extract the tarball, install the package to a staging dir, then copy out only
    # the files we need (dist, package.json, the CLI). Remove node_modules to keep the
    # image small — the bundled binary does not need it.
    mkdir -p /tmp/pi-stage; \
    tar xf /tmp/pi.tgz -C /tmp/pi-stage; \
    cp -r /tmp/pi-stage/package/dist /usr/local/lib/pi/dist; \
    cp /tmp/pi-stage/package/package.json /usr/local/lib/pi/; \
    rm -rf /tmp/pi-stage /tmp/pi.tgz; \
    # Create a thin CLI shim that runs the bundled ESM binary.
    printf '#!/bin/sh\nexec node /usr/local/lib/pi/dist/bundle/cli.js "$@"\n' > /usr/local/bin/pi; \
    chmod 0755 /usr/local/bin/pi; \
    pi --version

# Prepare.d scripts run at sandbox start-up via boks-prepare.
# This one discovers model servers on opened host ports and writes models.json.
COPY --chmod=0755 prepare.d/ /etc/boks/prepare.d/

USER 1000:1000
WORKDIR /home/agent

CMD ["pi"]
