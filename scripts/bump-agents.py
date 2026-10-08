#!/usr/bin/env python3
"""Move every agent image's pinned CLI to the vendor's latest release, digests included.

Each image pins a version and the sha256 of what it downloads, so an image is reproducible and
a tampered download fails the build. The cost is that nothing moves the pins: Claude Code sat
66 releases behind before this existed. This script moves them, taking each digest from where
the vendor publishes it, so a bump is a reviewable diff rather than a trust-on-first-use:

  claude        downloads.claude.ai's per-release manifest.json (what its installer checks)
  codex, copilot, docker-agent, gemini, opencode
                GitHub's own sha256 digest for each release asset
  droid         the .sha256 file Factory publishes beside each binary
  pi            the npm registry's sha512 integrity
  node          the version only: the base image checks SHASUMS256.txt from nodejs.org at build
  cursor        Cursor publishes no digest, so the download is hashed here, as the current
                pin was. This one is trust-on-first-use, and says so in its output.

Usage:
  scripts/bump-agents.py            rewrite the Dockerfiles that are behind
  scripts/bump-agents.py --check    report only; exit 1 when anything is behind

Needs network access and `gh` (authenticated) for the GitHub digests.
"""
import hashlib
import json
import re
import subprocess
import sys
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent


def fetch(url):
    req = urllib.request.Request(url, headers={"User-Agent": "boks-bump-agents"})
    with urllib.request.urlopen(req, timeout=120) as r:
        return r.read()


def fetch_json(url):
    return json.loads(fetch(url))


def gh_release(repo, tag=None):
    path = f"repos/{repo}/releases/" + (f"tags/{tag}" if tag else "latest")
    out = subprocess.run(["gh", "api", path], check=True, capture_output=True, text=True).stdout
    return json.loads(out)


def gh_digest(release, asset):
    for a in release["assets"]:
        if a["name"] == asset:
            digest = a.get("digest") or ""
            if not digest.startswith("sha256:"):
                raise SystemExit(f"{asset}: GitHub publishes no sha256 digest for it")
            return digest.removeprefix("sha256:")
    raise SystemExit(f"{release['tag_name']}: no asset named {asset}")


def npm_latest(package):
    return fetch_json(f"https://registry.npmjs.org/{package}/latest")


# Each agent: the Dockerfile, its version ARG, and a function from nothing to
# (version, {ARG name: value}) for everything the bump rewrites besides the version.

def claude():
    version = fetch(
        "https://downloads.claude.ai/claude-code-releases/latest").decode().strip()
    m = fetch_json(f"https://downloads.claude.ai/claude-code-releases/{version}/manifest.json")
    p = m["platforms"]
    return version, {"CLAUDE_CODE_SHA256_AMD64": p["linux-x64"]["checksum"],
                     "CLAUDE_CODE_SHA256_ARM64": p["linux-arm64"]["checksum"]}


def codex():
    r = gh_release("openai/codex")
    version = r["tag_name"].removeprefix("rust-v")
    return version, {"CODEX_SHA256_AMD64": gh_digest(r, "codex-x86_64-unknown-linux-musl.tar.gz"),
                     "CODEX_SHA256_ARM64": gh_digest(r, "codex-aarch64-unknown-linux-musl.tar.gz")}


def copilot():
    r = gh_release("github/copilot-cli")
    version = r["tag_name"].removeprefix("v")
    return version, {"COPILOT_SHA256_AMD64": gh_digest(r, "copilot-linux-x64.tar.gz"),
                     "COPILOT_SHA256_ARM64": gh_digest(r, "copilot-linux-arm64.tar.gz")}


def docker_agent():
    r = gh_release("docker/docker-agent")
    version = r["tag_name"].removeprefix("v")
    return version, {"DOCKER_AGENT_SHA256_AMD64": gh_digest(r, "docker-agent-linux-amd64"),
                     "DOCKER_AGENT_SHA256_ARM64": gh_digest(r, "docker-agent-linux-arm64")}


def gemini():
    r = gh_release("google-gemini/gemini-cli")
    version = r["tag_name"].removeprefix("v")
    return version, {"GEMINI_CLI_SHA256": gh_digest(r, "gemini-cli-bundle.zip")}


def opencode():
    r = gh_release("sst/opencode")
    version = r["tag_name"].removeprefix("v")
    return version, {"OPENCODE_SHA256_AMD64": gh_digest(r, "opencode-linux-x64-baseline.tar.gz"),
                     "OPENCODE_SHA256_ARM64": gh_digest(r, "opencode-linux-arm64.tar.gz")}


def droid():
    version = fetch("https://downloads.factory.ai/factory-cli/LATEST").decode().strip()
    base = f"https://downloads.factory.ai/factory-cli/releases/{version}/linux"
    sums = {}
    for arg, arch in (("DROID_SHA256_AMD64", "x64"), ("DROID_SHA256_ARM64", "arm64")):
        sums[arg] = fetch(f"{base}/{arch}/droid.sha256").decode().split()[0]
    return version, sums


def pi():
    m = npm_latest("@earendil-works/pi-coding-agent")
    return m["version"], {"PI_INTEGRITY": m["dist"]["integrity"]}


def node():
    releases = fetch_json("https://nodejs.org/dist/index.json")
    current = re.search(r"^ARG NODE_VERSION=(\d+)\.", (ROOT / "images/base/Dockerfile").read_text(), re.M)
    major = f"v{current.group(1)}."
    latest = next(r["version"] for r in releases if r["version"].startswith(major) and r["lts"])
    return latest.removeprefix("v"), {}


def cursor():
    script = fetch("https://cursor.com/install").decode()
    found = re.findall(r"lab/(\d{4}\.\d{2}\.\d{2}-[0-9a-f]+)", script)
    if not found:
        raise SystemExit("cursor: the install script names no version")
    version = found[0]
    sums = {}
    for arg, arch in (("CURSOR_AGENT_SHA256_AMD64", "x64"), ("CURSOR_AGENT_SHA256_ARM64", "arm64")):
        blob = fetch(f"https://downloads.cursor.com/lab/{version}/linux/{arch}/agent-cli-package.tar.gz")
        sums[arg] = hashlib.sha256(blob).hexdigest()
    return version, sums


AGENTS = [
    ("claude", "images/claude/Dockerfile", "CLAUDE_CODE_VERSION", claude),
    ("codex", "images/codex/Dockerfile", "CODEX_VERSION", codex),
    ("copilot", "images/copilot/Dockerfile", "COPILOT_VERSION", copilot),
    ("cursor", "images/cursor/Dockerfile", "CURSOR_AGENT_VERSION", cursor),
    ("docker-agent", "images/docker-agent/Dockerfile", "DOCKER_AGENT_VERSION", docker_agent),
    ("droid", "images/droid/Dockerfile", "DROID_VERSION", droid),
    ("gemini", "images/gemini/Dockerfile", "GEMINI_CLI_VERSION", gemini),
    ("opencode", "images/opencode/Dockerfile", "OPENCODE_VERSION", opencode),
    ("pi", "images/pi/Dockerfile", "PI_VERSION", pi),
    ("node", "images/base/Dockerfile", "NODE_VERSION", node),
]


def pinned(text, arg):
    m = re.search(rf"^ARG {arg}=(\S+)$", text, re.M)
    if not m:
        raise SystemExit(f"no 'ARG {arg}=' line")
    return m.group(1)


def main():
    check = "--check" in sys.argv[1:]
    behind, failed = [], []
    for name, rel, version_arg, latest in AGENTS:
        path = ROOT / rel
        text = path.read_text()
        current = pinned(text, version_arg)
        try:
            version, args = latest()
        except (Exception, SystemExit) as e:  # one vendor's outage must not hide the others
            failed.append(name)
            print(f"{name:13} {current:24} ?  could not read the latest release: {e}")
            continue
        if version == current:
            print(f"{name:13} {current:24} up to date")
            continue
        behind.append(name)
        note = "  (digest computed here: Cursor publishes none)" if name == "cursor" else ""
        print(f"{name:13} {current:24} -> {version}{note}")
        if check:
            continue
        for arg, value in {version_arg: version, **args}.items():
            pinned(text, arg)
            text = re.sub(rf"^ARG {arg}=\S+$", f"ARG {arg}={value}", text, flags=re.M)
        path.write_text(text)
    if failed:
        print(f"\ncould not check: {', '.join(failed)}", file=sys.stderr)
    if check and behind:
        sys.exit(1)
    if failed:
        sys.exit(2)


if __name__ == "__main__":
    main()
