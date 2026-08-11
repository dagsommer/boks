BINARY  := bin/boks
PKG     := github.com/dagsommer/boks
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X $(PKG)/internal/cli.Version=$(VERSION)

.PHONY: build test check integration vet fmt clean

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
