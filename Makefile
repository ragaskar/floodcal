GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.0.0-dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
SHA256  ?= shasum -a 256

PLATFORMS := darwin/arm64 linux/amd64 linux/arm64

.PHONY: build dist test smoke clean

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/floodcal ./cmd/floodcal

test:
	$(GO) test ./...

dist:
	rm -rf dist && mkdir -p dist
	@set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		name=floodcal_$(VERSION)_$${os}_$${arch}; \
		echo "building $$name"; \
		mkdir -p dist/$$name; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/$$name/floodcal ./cmd/floodcal; \
		cp configs/example-config.yaml dist/$$name/; \
		tar -C dist -czf dist/$$name.tar.gz $$name; \
		rm -rf dist/$$name; \
	done
	cd dist && $(SHA256) *.tar.gz > checksums.txt

# Live smoke tests against a real Google Calendar (creates and deletes
# clearly labelled test events; never touches real floodcal events).
# Usage: make smoke SMOKE_CONFIG=path/to/config.yaml
# The test binary is built at a fixed path (bin/floodcal-smoke.test) so an
# outbound firewall only needs to allow it once.
smoke:
	@test -n "$(SMOKE_CONFIG)" || { echo "usage: make smoke SMOKE_CONFIG=path/to/config.yaml" >&2; exit 1; }
	$(GO) test -c -tags live -o bin/floodcal-smoke.test ./internal/gcal/
	FLOODCAL_SMOKE_CONFIG=$(abspath $(SMOKE_CONFIG)) bin/floodcal-smoke.test -test.v -test.count=1 -test.run '^TestLive'

clean:
	rm -rf bin dist
