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

# The platforms a release ships. darwin/arm64 is the verified one; Linux builds and has
# never booted a sandbox. There is no windows or darwin/amd64 target on purpose — see the
# comment at the top of .github/workflows/release.yml.
RELEASE_TARGETS := darwin/arm64 linux/amd64 linux/arm64

.PHONY: build test check integration vet fmt clean dist images images-test image-base $(addprefix image-,$(AGENT_IMAGES)) $(addprefix image-test-,$(AGENT_IMAGES))

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
