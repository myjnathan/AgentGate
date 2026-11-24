# AgentGate build entry points (layout follows dsh-ee's Makefile style).
#
#   make build     server + ag-setup for this machine -> dist/
#   make release   linux/amd64 + linux/arm64 static binaries -> dist/
#   make test      go vet + full test suite
#   make smoke     build, boot demo mode, run e2e/smoke.py against it
#   make image     docker image agentgate:$(VERSION)
#   make bundle      offline management bundle (image tar + compose + install.sh + VERSION)
#   make bundle-user offline user bundle zip (ag-guard + ag-setup + guard.json + MANIFEST.json)
#   make clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO      ?= go
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build release test smoke image bundle bundle-user clean

bundle-user: build
	@python3 deploy/make-user-bundle.py

build:
	mkdir -p dist
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/agentgate ./cmd/agentgate
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o dist/ag-setup ./cmd/ag-setup
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o dist/ag-guard ./cmd/ag-guard
	@echo "dist/agentgate ($(VERSION))"

release:
	mkdir -p dist
	for arch in amd64 arm64; do \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/agentgate-linux-$$arch ./cmd/agentgate || exit 1; \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags "-s -w" -o dist/ag-guard-linux-$$arch ./cmd/ag-guard || exit 1; \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags "-s -w" -o dist/ag-setup-linux-$$arch ./cmd/ag-setup || exit 1; \
	done

test:
	$(GO) vet ./... && $(GO) test ./...

smoke: build
	@AGENTGATE_MASTER_KEY=make-smoke-master-key AGENTGATE_DEMO=1 OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 AGENTGATE_DATA=$$(mktemp -d) AGENTGATE_ADDR=127.0.0.1:8799 AGENTGATE_FSYNC=$${AGENTGATE_FSYNC:-} ./dist/agentgate & \
	SRV=$$!; sleep 2; \
	AGENTGATE_GATEWAY=http://127.0.0.1:8799 AGENTGATE_GUARD_BANNED_WORDS=test_sensitive ./dist/ag-guard -listen 127.0.0.1:18765 & \
	GUARD=$$!; sleep 1; \
	AGENTGATE_BASE=http://127.0.0.1:8799 AGENTGATE_SIDECAR=http://127.0.0.1:18765 python3 e2e/smoke.py; RC=$$?; kill $$SRV $$GUARD; exit $$RC

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
