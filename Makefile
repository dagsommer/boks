BINARY  := bin/boks
PKG     := github.com/dagsommer/boks
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X $(PKG)/internal/cli.Version=$(VERSION)

# The agent images. IMAGE_TAG is read out of the Go registry rather than repeated here:
# internal/agent is what a running boks resolves an agent to, so it is the source of truth,
# and a Makefile with its own copy would eventually disagree with it. The images and release
# workflows call the same script, which additionally refuses a git tag that does not match.
IMAGE_REPO := ghcr.io/dagsommer/boks
IMAGE_TAG  := $(shell scripts/image-tag.sh)
# Every directory under images/ except the base, which everything else builds on.
AGENT_IMAGES := $(filter-out base,$(notdir $(wildcard images/*)))

# `docker-agent version`, not `docker-agent --version`; everything else takes the flag.
VERSION_ARG_docker-agent := version

# The platforms a release ships, and it must stay the same set as the build matrix in
# .github/workflows/release.yml — `make dist` exists so a release can be reproduced off
# GitHub, which it cannot do if it builds a different set. All three of these have booted a
# sandbox: darwin/arm64 on 2026-08-13, linux/amd64 and windows/amd64 on 2026-08-15. See
# docs/verification.md, and the comment at the top of release.yml for why darwin/amd64 and
# windows/arm64 are absent.
RELEASE_TARGETS := darwin/arm64 linux/amd64 linux/arm64 windows/amd64

.PHONY: build test check integration vet fmt clean dist docs docs-check release-notes winget images images-test image-base $(addprefix image-,$(AGENT_IMAGES)) $(addprefix image-test-,$(AGENT_IMAGES))

build:
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/boks

test:
	go test ./...

vet:
	go vet ./...

check: vet test

# Drives a real containerd. Runs against the isolating runtime by default, so a pass
# means the assertions held behind a VM boundary. See docs/verification.md.
integration:
	BOKS_INTEGRATION=1 go test ./internal/sandbox/ -run Integration -v

fmt:
	gofmt -l -w .

clean:
	rm -rf bin dist

# --- release artifacts ------------------------------------------------------------
#
# The same scripts CI runs, run locally, because a release nobody can reproduce outside
# GitHub Actions is not a verifiable artefact. Nothing in Boks uses cgo, so every target
# cross-compiles from any host and the tarballs are byte-identical between runs of one
# commit: build this twice and the checksums match.
#
# .deb and .rpm are not built here — they need dpkg-deb and rpmbuild, which not every host
# has. Run scripts/package-linux.sh <arch> where they do.
dist:
	@for target in $(RELEASE_TARGETS); do \
		scripts/build-release.sh "$${target%/*}" "$${target#*/}" || exit 1; \
	done

# --- generated documentation ------------------------------------------------------
#
# docs/cli.md is rendered from the cobra command tree, so it describes the CLI that exists
# rather than the one somebody wrote about. Run this after changing a command, a flag or a
# help string; `make test` fails until you have, which is what stops the page from ageing.

docs:
	go run ./cmd/gen-docs

docs-check:
	go run ./cmd/gen-docs -check

# Release notes for a tag, from the conventional commits since the previous one. Prints to
# stdout so you can read it before it lands; add --insert to write it into
# docs/release-notes.md. See the top of the script, and docs/release-notes.md, for how a
# release is cut and why the first entry is hand-written.
#
#   make release-notes TAG=v0.1.1
#   make release-notes TAG=v0.1.1 INSERT=--insert
release-notes:
	@test -n "$(TAG)" || { echo "usage: make release-notes TAG=vX.Y.Z [INSERT=--insert]" >&2; exit 2; }
	./scripts/release-notes.sh $(INSERT) $(TAG)

# Bump the version, commit, tag, and push. The tag push triggers release.yml, which builds
# and publishes the GitHub release; homebrew.yml then fires on the published event and
# updates the tap. `brew upgrade boks` picks it up from there.
#
# Usage: make release VERSION=0.1.2
release:
	@test -n "$(VERSION)" || { echo "usage: make release VERSION=x.y.z" >&2; exit 2; }
	@git diff --quiet && git diff --cached --quiet || { echo "release: working tree is dirty; commit or stash first" >&2; exit 2; }
	perl -i -pe 's/^(const ImageTag = )"[^"]*"/$${1}"$(VERSION)"/' internal/agent/agent.go
	git add internal/agent/agent.go
	git commit -m "release: $(VERSION)"
	git tag v$(VERSION)
	@echo ""
	@echo "v$(VERSION) committed and tagged. To publish:"
	@echo "  git push origin main && git push origin v$(VERSION)"

# --- winget manifests ---------------------------------------------------------------
#
# Renders packaging/winget/manifests/*.yaml.in and checks the result against winget's own
# JSON schemas, which validate.py fetches. It needs python3 with jsonschema and pyyaml, and
# network access for the first fetch; .github/workflows/winget.yml runs the same two scripts
# with the same controls on every change under packaging/winget/.
#
# The version and digest are placeholders unless passed, because this target exists to check
# the manifests are well formed, not to stamp a release — for that, pass the real archive and
# `render.sh` computes the digest from it:
#
#   make winget WINGET_VERSION=0.1.0 WINGET_DIGEST=dist/boks_0.1.0_windows_amd64.zip
#
# Not named VERSION/DIGEST: VERSION is this Makefile's own `git describe` and overriding it
# on the command line would quietly change what every other target stamps into the binary.
#
# Nothing here is a substitute for `winget validate`, which runs only on Windows and has
# never been run against these files. See packaging/winget/README.md.
WINGET_VERSION ?= 0.0.0
WINGET_DIGEST  ?= A1B2C3D4E5F60718293A4B5C6D7E8F90A1B2C3D4E5F60718293A4B5C6D7E8F90
winget:
	packaging/winget/render.sh $(WINGET_VERSION) $(WINGET_DIGEST) dist/winget

# --- agent images -----------------------------------------------------------------
#
# These build for the host's architecture only. Multi-arch belongs to CI, which has a runner
# per architecture; emulating the other one here would take an hour and prove nothing that
# the native build does not.

images: image-base $(addprefix image-,$(AGENT_IMAGES))

image-base:
	docker build -t $(IMAGE_REPO)/base:$(IMAGE_TAG) images/base

# Each agent image is a thin layer on the base, so the local base is what it builds against
# — otherwise the first build would try to pull a published image that may not exist yet.
$(addprefix image-,$(AGENT_IMAGES)): image-%: image-base
	docker build --build-arg BASE_IMAGE=$(IMAGE_REPO)/base:$(IMAGE_TAG) \
		-t $(IMAGE_REPO)/$*:$(IMAGE_TAG) images/$*

# What a plain `docker run` can actually establish: the CLI is installed and starts, the
# process is uid 1000 under tini, and an injected CA reaches the trust store. It says
# nothing about isolation — that needs a hypervisor and `make integration`.
images-test: image-test-base $(addprefix image-test-,$(AGENT_IMAGES))

.PHONY: image-test-base
image-test-base:
	@echo '--- base: uid, init and CA ---'
	docker run --rm $(IMAGE_REPO)/base:$(IMAGE_TAG) id
	docker run --rm $(IMAGE_REPO)/base:$(IMAGE_TAG) cat /proc/1/comm
	./$(BINARY) ca show -create >/dev/null
	./$(BINARY) ca env | docker run --rm -i --env-file /dev/stdin $(IMAGE_REPO)/base:$(IMAGE_TAG) \
		openssl x509 -in /usr/local/share/ca-certificates/boks-local-ca.crt -noout -subject

$(addprefix image-test-,$(AGENT_IMAGES)): image-test-%:
	@echo '--- $*: the CLI runs ---'
	docker run --rm $(IMAGE_REPO)/$*:$(IMAGE_TAG) \
		$$(docker image inspect $(IMAGE_REPO)/$*:$(IMAGE_TAG) --format '{{index .Config.Cmd 0}}') \
		$(or $(VERSION_ARG_$*),--version)
