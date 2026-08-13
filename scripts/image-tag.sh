#!/usr/bin/env bash
#
# Print the version Boks releases under, and refuse a git tag that disagrees with it.
#
# There is exactly one version in this project, and it lives in internal/agent as
# `ImageTag`, because that constant is what a *running* boks resolves an agent to. A
# release whose binary points at ghcr.io/dagsommer/boks/claude:0.1.0 while the release is
# called v0.2.0 is a release that lies about itself, so the tag and the constant are checked
# against each other in one place — here — and everything that needs the version calls this
# rather than keeping its own copy of the sed.
#
# Callers: .github/workflows/images.yml, .github/workflows/release.yml, the Makefile.
#
# In GitHub Actions the check is automatic: GITHUB_REF_TYPE and GITHUB_REF_NAME are set by
# the runner, so a tag push that disagrees fails the job before anything is built or
# published. Outside Actions those variables are unset and this just prints the version.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source_file="$root/internal/agent/agent.go"

image_tag="$(sed -n 's/^const ImageTag = "\(.*\)"$/\1/p' "$source_file")"
if [ -z "$image_tag" ]; then
	echo "could not read ImageTag from internal/agent/agent.go" >&2
	exit 1
fi

# A release tag that disagrees with the constant would publish images the built binary does
# not point at, and a binary that reports a version nothing was published under. Fail here
# rather than ship that.
if [ "${GITHUB_REF_TYPE:-}" = tag ] && [ "${GITHUB_REF_NAME#v}" != "$image_tag" ]; then
	echo "git tag ${GITHUB_REF_NAME} does not match agent.ImageTag ${image_tag}" >&2
	exit 1
fi

printf '%s\n' "$image_tag"
