# AgentGate build entry points (layout follows dsh-ee's Makefile style).
#
#   make build     server + ag-setup for this machine -> dist/
#   make release   linux/amd64 + linux/arm64 static binaries -> dist/
#   make test      go vet + full test suite
#   make smoke     build, boot demo mode, run e2e/smoke.py against it
#   make image     docker image agentgate:$(VERSION)
#   make bundle    offline bundle (image tar + compose + install.sh + VERSION)
#   make clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO      ?= go
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build release test smoke image bundle clean

build:
	mkdir -p dist
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/agentgate ./cmd/agentgate
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o dist/ag-setup ./cmd/ag-setup
	@echo "dist/agentgate ($(VERSION))"

release:
	mkdir -p dist
	for arch in amd64 arm64; do \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/agentgate-linux-$$arch ./cmd/agentgate || exit 1; \
	done

test:
	$(GO) vet ./... && $(GO) test ./...

smoke: build
	@AGENTGATE_MASTER_KEY=make-smoke-master-key AGENTGATE_DEMO=1 AGENTGATE_DATA=$$(mktemp -d) AGENTGATE_ADDR=127.0.0.1:8799 ./dist/agentgate & \
	SRV=$$!; sleep 2; AGENTGATE_BASE=http://127.0.0.1:8799 python3 e2e/smoke.py; RC=$$?; kill $$SRV; exit $$RC

image:
	docker build --build-arg VERSION=$(VERSION) -t agentgate:$(VERSION) .

bundle: image
	mkdir -p dist/bundle
	docker save agentgate:$(VERSION) | gzip > dist/bundle/images.tar.gz
	cp deploy/compose.yml deploy/install.sh dist/bundle/
	echo "$(VERSION)" > dist/bundle/VERSION
	@echo "offline bundle in dist/bundle/"

clean:
	rm -rf dist
