BINARY  := bin/boks
PKG     := github.com/dagsommer/boks
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X $(PKG)/internal/cli.Version=$(VERSION)

# The agent images. IMAGE_TAG is read out of the Go registry rather than repeated here:
# internal/agent is what a running boks resolves an agent to, so it is the source of truth,
# and a Makefile with its own copy would eventually disagree with it. The release workflow
# reads the same line and refuses to publish when a git tag does not match it.
IMAGE_REPO := ghcr.io/dagsommer/boks
IMAGE_TAG  := $(shell sed -n 's/^const ImageTag = "\(.*\)"$$/\1/p' internal/agent/agent.go)
# Every directory under images/ except the base, which everything else builds on.
AGENT_IMAGES := $(filter-out base,$(notdir $(wildcard images/*)))

# `docker-agent version`, not `docker-agent --version`; everything else takes the flag.
VERSION_ARG_docker-agent := version

.PHONY: build test check integration vet fmt clean docs docs-check release-notes images images-test image-base $(addprefix image-,$(AGENT_IMAGES)) $(addprefix image-test-,$(AGENT_IMAGES))

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
	rm -rf bin

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
