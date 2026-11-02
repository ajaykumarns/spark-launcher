GO ?= go

# Build flags and linker flags are separate: -buildvcs and -trimpath are go
# build flags, not -ldflags arguments.
GOFLAGS := -trimpath -buildvcs=false
GOLDFLAGS := -s -w
PLATFORMS := darwin/arm64 linux/arm64

.PHONY: build installer crossbuild menubar menubar-headless package-menubar test race vet fmt test-python-reference clean

# Always invoke the Go build: dist/ artifacts are not tracked as target
# prerequisites, so a stale binary is never silently reused. Go's build cache
# makes repeated invocation cheap.
build:
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(GOLDFLAGS)' -o dist/sparky ./cmd/sparky
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(GOLDFLAGS)' -o dist/sparky-install ./cmd/sparky-install

installer:
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(GOLDFLAGS)' -o dist/sparky-install ./cmd/sparky-install

# Standalone native binaries for the Mac and both Sparks. Any compiler
# failure fails the whole target; later successes never mask earlier ones.
crossbuild:
	set -eu; \
	for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		for cmdname in sparky sparky-install; do \
			echo "BUILD $$os/$$arch $$cmdname"; \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build $(GOFLAGS) \
				-ldflags '$(GOLDFLAGS)' -o dist/$$os-$$arch/$$cmdname ./cmd/$$cmdname || exit 1; \
		done; \
	done

# Native macOS menu bar app. Only the GUI uses cgo; the CLI/install builds
# above stay CGO_ENABLED=0. The .app bundles a standalone copy of the CLI.
menubar:
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(GOLDFLAGS)' -o dist/sparky ./cmd/sparky
	CGO_ENABLED=1 GOOS=darwin $(GO) build $(GOFLAGS) -ldflags '$(GOLDFLAGS)' -o dist/sparky-menubar ./cmd/sparky-menubar
	sh tools/package-menubar.sh dist/Sparky.app

# Headless check: the menubar command must still build without cgo, so the
# CLI/install story and non-macOS builds never depend on AppKit.
menubar-headless:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags '$(GOLDFLAGS)' -o dist/darwin-arm64/sparky-menubar-stub ./cmd/sparky-menubar
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags '$(GOLDFLAGS)' -o dist/linux-arm64/sparky-menubar-stub ./cmd/sparky-menubar

# Rebuild only the .app bundle from existing artifacts (reproducible).
package-menubar:
	sh tools/package-menubar.sh dist/Sparky.app

test:
	$(GO) test ./...

race:
	$(GO) test -race -count=1 -timeout=120s ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w cmd internal

# Reference only: run the original Python tests against the legacy source tree.
# This is for rollback comparison; production is the Go build above.
test-python-reference:
	PYTHONPATH=src python3 -m unittest discover -s tests

clean:
	rm -rf dist
