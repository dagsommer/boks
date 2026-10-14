#!/bin/sh
# Render bucket/boks.json, the Scoop manifest, for one release.
#
#   packaging/scoop/render.sh VERSION path/to/SHA256SUMS
#
# The repository is its own Scoop bucket: `scoop bucket add boks
# https://github.com/dagsommer/boks` clones it and reads bucket/. .github/workflows/scoop.yml
# runs this when a release is published and commits the result to main, the way homebrew.yml
# renders the tap. The hash is the release's own SHA256SUMS entry for the Windows zip, so
# Scoop refuses an archive whose bytes differ from what the release built.
#
# The manifest puts only boks.exe on PATH (Scoop's shim), not the directory with containerd
# and ctr beside it — the thing winget cannot avoid on a machine without symlinks; see
# packaging/winget/README.md. Boks finds its runtime from its own executable's directory.
set -eu

version="$1"
sums="$2"
zip="boks_${version}_windows_amd64.zip"
hash="$(awk -v f="$zip" '$2 == f { print $1 }' "$sums")"
[ -n "$hash" ] || { echo "render.sh: no SHA256SUMS entry for $zip" >&2; exit 1; }

root="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$root/bucket"
cat >"$root/bucket/boks.json" <<JSON
{
    "version": "${version}",
    "description": "Run AI coding agents and other untrusted developer tooling in isolated microVMs.",
    "homepage": "https://dagsommer.github.io/boks",
    "license": "Apache-2.0",
    "notes": [
        "Boks needs the Windows Hypervisor Platform. If it is not enabled, run in an elevated PowerShell:",
        "  Enable-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform -All",
        "then restart, and check with: boks doctor"
    ],
    "architecture": {
        "64bit": {
            "url": "https://github.com/dagsommer/boks/releases/download/v${version}/${zip}",
            "hash": "${hash}",
            "extract_dir": "boks_${version}_windows_amd64"
        }
    },
    "bin": "boks.exe",
    "checkver": {
        "github": "https://github.com/dagsommer/boks"
    },
    "autoupdate": {
        "architecture": {
            "64bit": {
                "url": "https://github.com/dagsommer/boks/releases/download/v\$version/boks_\$version_windows_amd64.zip",
                "extract_dir": "boks_\$version_windows_amd64"
            }
        },
        "hash": {
            "url": "https://github.com/dagsommer/boks/releases/download/v\$version/SHA256SUMS",
            "regex": "\$sha256\\\\s+\$basename"
        }
    }
}
JSON
echo "rendered bucket/boks.json for ${version}"
