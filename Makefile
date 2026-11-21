GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.0.0-dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
SHA256  ?= shasum -a 256

PLATFORMS := darwin/arm64 linux/amd64 linux/arm64

.PHONY: build dist test clean

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

clean:
	rm -rf bin dist
