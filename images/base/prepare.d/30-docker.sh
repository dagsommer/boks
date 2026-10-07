#!/bin/sh
# Start the Docker engine inside the sandbox, so `docker` works for the agent without sudo.
#
# The sandbox's container is privileged inside its VM (internal/sandbox, withPrivileged): the
# VM is the boundary, and the engine needs a writable cgroup v2 and /dev/fuse, which that
# provides. This starts dockerd once, in the background — it outlives the agent's own process
# and is reused by the next one — opens its socket to the agent, and points containers at the
# sandbox's proxy so their traffic goes where the sandbox's own does.
#
# Never fatal: a sandbox that cannot run Docker still runs its agent (boks-prepare steps over a
# failure). BOKS_NO_DOCKER=1 skips it.
set -u

[ -z "${BOKS_NO_DOCKER:-}" ] || exit 0
command -v dockerd >/dev/null 2>&1 || exit 0
docker_sock=/var/run/docker.sock

if ! docker info >/dev/null 2>&1; then
	# A sandbox created before privileged sandboxes has no writable cgroup v2; the engine
	# would only fail, and say so less clearly than this.
	if [ ! -w /sys/fs/cgroup ] && ! sudo -n test -w /sys/fs/cgroup/cgroup.controllers 2>/dev/null; then
		echo "boks: docker is not available in this sandbox: it was created before sandboxes were privileged; recreate it to use docker" >&2
		exit 0
	fi
	# sudo keeps the proxy (images/base/Dockerfile, env_keep), so pulls go through it.
	sudo -n sh -c 'nohup dockerd >/var/log/dockerd.log 2>&1 &' || exit 0
	i=0
	while [ ! -S "$docker_sock" ] && [ "$i" -lt 40 ]; do
		sleep 0.25
		i=$((i + 1))
	done
	[ -S "$docker_sock" ] || { echo "boks: dockerd did not start; see /var/log/dockerd.log" >&2; exit 0; }
fi

# The agent's uid is not known when the image is built (it may be the host's), so the socket is
# opened to everyone in this single-user VM rather than to a group.
sudo -n chmod 666 "$docker_sock" 2>/dev/null || true

# Containers and builds inherit the proxy, so their traffic is judged like the sandbox's own.
if [ -n "${HTTPS_PROXY:-}${HTTP_PROXY:-}" ] && [ -n "${HOME:-}" ] && [ "$HOME" != "/" ] && [ ! -e "$HOME/.docker/config.json" ]; then
	mkdir -p "$HOME/.docker" 2>/dev/null &&
		python3 - "$HOME/.docker/config.json" <<'PYEOF' || true
import json, os, sys
proxies = {k: v for k, v in {
    "httpProxy": os.environ.get("HTTP_PROXY", ""),
    "httpsProxy": os.environ.get("HTTPS_PROXY", ""),
    "noProxy": os.environ.get("NO_PROXY", ""),
}.items() if v}
with open(sys.argv[1], "w") as f:
    json.dump({"proxies": {"default": proxies}}, f, indent=2)
    f.write("\n")
PYEOF
fi
exit 0
