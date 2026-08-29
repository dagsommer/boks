# The shared foundation every Boks agent image is built on.
#
# Debian 13 (trixie) rather than Ubuntu: it is the smaller of the two slim images, its
# glibc is 2.41 (newer than Ubuntu 24.04's 2.39, which matters because some vendor CLIs
# ship glibc-linked binaries with a floor as high as 2.39 on arm64), and — unlike
# ubuntu:24.04 — it does not already occupy uid 1000 with a distro user, so `agent` can
# have the uid the kit convention expects without deleting someone first.
FROM debian:trixie-slim

# ARG, not ENV: noninteractive is a property of this build, not of a sandbox someone later
# runs an editor in.
ARG DEBIAN_FRONTEND=noninteractive

# One layer for the whole distro payload. It is the expensive step and the one that changes
# least, so it goes first and stays cacheable; splitting it would only add layers that are
# always invalidated together anyway.
#
# tini is here because a long-lived sandbox needs an init: a real Docker Sandboxes guest was
# observed running tini as PID 1, and without one an agent that spawns and abandons children
# accumulates zombies for the life of the sandbox.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      build-essential \
      ca-certificates \
      curl \
      git \
      gnupg \
      jq \
      less \
      libatomic1 \
      openssh-client \
      procps \
      python3 \
      python3-pip \
      python3-venv \
      ripgrep \
      tini \
      unzip \
      xz-utils \
 && rm -rf /var/lib/apt/lists/*

# Node from nodejs.org rather than the distro, because the agent CLIs are published to npm
# and track Node LTS, while Debian's node is pinned to whatever was current at freeze. The
# version is an ARG so a release can move it without editing the recipe.
ARG NODE_VERSION=24.19.0
RUN set -eux; \
    case "$(dpkg --print-architecture)" in \
      amd64) node_arch=x64 ;; \
      arm64) node_arch=arm64 ;; \
      *) echo "boks: unsupported architecture $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac; \
    tarball="node-v${NODE_VERSION}-linux-${node_arch}.tar.xz"; \
    curl -fsSL -o /tmp/node.tar.xz "https://nodejs.org/dist/v${NODE_VERSION}/${tarball}"; \
    curl -fsSL -o /tmp/SHASUMS256.txt "https://nodejs.org/dist/v${NODE_VERSION}/SHASUMS256.txt"; \
    sum="$(awk -v f="$tarball" '$2 == f { print $1 }' /tmp/SHASUMS256.txt)"; \
    test -n "$sum"; \
    echo "$sum  /tmp/node.tar.xz" | sha256sum -c -; \
    tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 --no-same-owner; \
    rm -f /tmp/node.tar.xz /tmp/SHASUMS256.txt; \
    # 65 MB of C++ headers for building native addons. node-gyp fetches its own copy for
    # whatever version it is targeting, so keeping these only makes the image bigger.
    rm -rf /usr/local/include/node /usr/local/share/doc /usr/local/share/man; \
    node --version; \
    npm --version

# uid/gid 1000 with home /home/agent. This is the kit convention (`setup.startup` defaults
# to user "1000") and what a real sbx guest runs as, so a workspace written inside a sandbox
# ends up owned by the same uid as on a typical single-user Linux host.
RUN set -eux; \
    groupadd --gid 1000 agent; \
    useradd --uid 1000 --gid 1000 --create-home --shell /bin/bash agent; \
    mkdir -p /home/agent/.local/bin /home/agent/.npm-global; \
    chown -R agent:agent /home/agent; \
    # Writable by whatever uid the sandbox actually runs as, which is not always this one.
    #
    # A shared workspace forces the guest process to run as the uid that owns the host files
    # — 501 on a Mac, not 1000 — because the guest kernel checks the real uid of a virtiofs
    # file and libkrun has no id mapping to paper over it (see withHostUser in
    # internal/sandbox). HOME is not on that share; it lives in this image. So without this
    # the agent can write the user's repository and not its own configuration, which breaks
    # every agent that keeps state — a login, a cache, a settings file.
    #
    # 0777 rather than a second user or an entrypoint that chowns as root. The guest is a
    # disposable VM whose entire filesystem is the agent's to ruin, and docs/security-model.md
    # says in as many words that in-guest permissions are not a boundary and nothing depends
    # on them. Spending a root entrypoint to defend a directory inside that would buy nothing
    # and cost the one thing worth keeping: no root in the chain at all.
    chmod -R 0777 /home/agent

# The CA entrypoint has to write to the trust store, and MODE rather than OWNER is what makes
# that possible, because the uid is not knowable when this image is built.
#
# This used to chown these paths to agent, on the stated grounds that "the container process
# is uid 1000". That stopped being true: a sandbox with a workspace runs as the uid that owns
# the workspace on the host, so that files the agent creates belong to the person who will
# open them afterwards (internal/sandbox/hostuser.go). On a Mac that is typically 502, which
# owns nothing here, and boks-install-ca failed with
#
#   cannot create /usr/local/share/ca-certificates/boks-local-ca.crt.tmp.5: Permission denied
#
# So: the same three paths update-ca-certificates writes — the drop-in directory, the
# directory it regenerates the bundle in, and the files it rewrites — made writable by whoever
# the process turns out to be. Same reasoning as `chmod -R 0777 /home/agent` above, and the
# same threat model: the guest is a disposable VM whose entire filesystem is already the
# agent's to ruin, and docs/security-model.md says in as many words that in-guest permissions
# are not a boundary.
RUN set -eux; \
    mkdir -p /usr/local/share/ca-certificates; \
    chmod 0777 /usr/local/share/ca-certificates /etc/ssl/certs; \
    chmod 0666 /etc/ssl/certs/ca-certificates.crt /etc/ca-certificates.conf

# /etc/passwd, for the same reason and with a sharper consequence.
#
# The uid a sandbox runs as is the one that owns the workspace on the host, so it has no entry
# here and getpwuid() fails for it. A surprising amount of software treats that as fatal rather
# than as missing metadata — OpenSSH refuses to start at all ("No user exists for uid 502"),
# taking ssh, scp, git-over-ssh and any deploy with it. prepare.d/05-host-uid.sh adds the entry
# at start-up, once the number is knowable, and it needs somewhere to write it.
#
# Writable rather than owned, since the uid is not known at build time.
#
# What that does and does not cost, stated rather than waved at. Debian ships /bin/su setuid
# root, and su trusts /etc/passwd: an agent that can write this file can give itself a
# password-less root entry and become root INSIDE THE GUEST. That is worth knowing and it is
# not an escape — the boundary is the VM, docs/security-model.md says in-guest permissions are
# not one, and the filesystem is already entirely the agent's to ruin. The one practical
# consequence is on the workspace share: a file created as root lands on the host owned by
# uid 0, which the person who opens it afterwards cannot delete.
#
# The alternative to accepting that is to drop the setuid bits in this image
# (`find / -perm /4000 -type f -exec chmod u-s {} +`), which a sandbox has no use for — there
# is no root login to su to. Not done here, because it is a separate decision from making ssh
# work and it deserves to be taken on its own.
RUN chmod 0666 /etc/passwd /etc/group

# The base image's own start-up scripts. Derived images add theirs to the same directory;
# boks-prepare runs whatever is there in name order, which is why these are numbered.
COPY --chmod=0755 prepare.d/ /etc/boks/prepare.d/

COPY --chmod=0755 boks-install-ca boks-entrypoint boks-open boks-prepare /usr/local/bin/

# The names a program reaches for when it wants a browser, all pointing at the one that says
# there isn't one. xdg-open is the Linux convention and `open` is the macOS one, which agents
# built for both platforms try in turn; BROWSER is what everything else honours. An agent's
# OAuth login is the case that matters, and before this it failed with the URL unprinted.
RUN set -eux; \
    ln -s boks-open /usr/local/bin/xdg-open; \
    ln -s boks-open /usr/local/bin/open; \
    mkdir -p /etc/boks/prepare.d
ENV BROWSER=/usr/local/bin/boks-open

# Node and Python carry their own trust stores and ignore the system one, so they need to be
# pointed at a bundle explicitly. All three name the *system* bundle rather than the injected
# certificate: that file always exists, so nothing breaks when no CA is injected, and
# update-ca-certificates appends the Boks CA to it in place when one is.
ENV NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt \
    REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt \
    SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt \
    NPM_CONFIG_PREFIX=/home/agent/.npm-global \
    PATH=/home/agent/.npm-global/bin:/home/agent/.local/bin:/usr/local/bin:/usr/local/sbin:/usr/bin:/usr/sbin:/bin:/sbin \
    LANG=C.UTF-8 \
    PIP_BREAK_SYSTEM_PACKAGES=1

# 1000:1000, not `agent`, and the numbers are load-bearing rather than a style choice.
#
# An image may name its user or number it, and only one of those survives the trip to a
# non-Linux host. Resolving `agent` to a uid means reading /etc/passwd out of this image's
# root filesystem, which is a Linux filesystem: containerd does it on Linux by mounting the
# snapshot, and on macOS it cannot mount it at all. Its answer there is to give up and store
# the string in Process.User.Username (spec_opts.go, `runtime.GOOS == "darwin"`), leaving
# Process.User.UID at zero — and nothing in the guest reads Username back, so `USER agent`
# meant **uid 0** on every Mac. Not as an error: the container simply ran as root.
#
# That is how a sandboxed agent ended up refusing to start, since Claude Code declines
# `--dangerously-skip-permissions` as root, which is the symptom that found this.
#
# Numbers need no filesystem, so every host honours them — see applyImageUser in
# internal/sandbox/imageconfig.go. uid/gid 1000 is the `agent` created above, and on Linux
# containerd still reads /etc/group for supplementary groups afterwards, of which this image
# has none.
#
# Keep it numeric. `USER agent` reads better and silently means root off Linux.
USER 1000:1000
WORKDIR /home/agent

# HOME, explicitly, because nothing else can supply it any more.
#
# This comment used to say HOME "still comes from this image's own /etc/passwd (crun calls
# set_home_env with the container uid, which finds /home/agent)". That was true while the
# container ran as 1000. A sandbox with a workspace now runs as the uid that owns that
# workspace on the host — 502 on a typical Mac — and no such user exists in this image's
# passwd, so crun finds nothing and leaves HOME as "/". The first thing to write a dotfile
# then fails on a path with a doubled slash:
#
#   PermissionError: [Errno 13] Permission denied: '//.claude.json.boks-tmp'
#
# An environment variable needs no passwd lookup, so it survives any uid. It has to be set
# after USER: an ENV before it would be inherited by the intervening RUN steps, which is
# harmless but misleading about where it takes effect.
#
# Images Boks does not build have the same exposure and no equivalent fix from here: an image
# that declares no HOME and is run as a foreign uid gets "/". internal/sandbox/hostuser.go
# says so where someone would look for it.
ENV HOME=/home/agent

# tini first so it reaps; the entrypoint installs the CA and then execs, so it does not
# linger as a process of its own.
#
# `-s` registers tini as a child subreaper. Inside a microVM the guest's own init is PID 1,
# so tini is not, and a tini that is neither PID 1 nor a subreaper reaps nothing — orphans go
# past it to PID 1. It also said so, in three lines of warning before every run, which read
# like a fault. `-s` is tini's own remedy for exactly that and makes the reaping real.
#
# Note that `boks run` replaces the whole argv (containerd's WithProcessArgs discards the
# image entrypoint), which is why internal/agent carries this same prefix in Agent.Init.
# The ENTRYPOINT here is what makes `docker run` and any other OCI runtime behave the same.
ENTRYPOINT ["/usr/bin/tini", "-s", "--", "/usr/local/bin/boks-entrypoint"]
CMD ["/bin/bash"]
