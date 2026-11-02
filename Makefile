GO ?= go

# Build flags and linker flags are separate: -buildvcs and -trimpath are go
# build flags, not -ldflags arguments.
GOFLAGS := -trimpath -buildvcs=false
GOLDFLAGS := -s -w
# Both are overridable: the release jobs build one platform per runner
# ("make crossbuild PLATFORMS=linux/arm64").
PLATFORMS ?= darwin/arm64 linux/arm64
# Per-package go test budget. CI passes 5m; the shorter default keeps the
# local target failing fast.
RACE_TIMEOUT ?= 120s
# Artifact directory (never committed; see .gitignore).
DIST ?= dist

# Release version metadata. The linker -X flag rewrites the package-level
# variables in internal/sparky/version.go, so they must stay variables and the
# version line must be rendered at run time, never assembled from a constant.
# They are deliberately empty by default: "make build", "make crossbuild" and
# "make menubar" keep printing the compiled-in development version. Only the
# release targets (and CI release jobs) pass VERSION=x.y.z COMMIT=<40-hex>.
VERSION ?=
COMMIT ?=
VERSION_LDFLAGS :=
ifneq ($(strip $(VERSION)),)
VERSION_LDFLAGS += -X sparky/internal/sparky.cliVersion=$(strip $(VERSION))
endif
ifneq ($(strip $(COMMIT)),)
VERSION_LDFLAGS += -X sparky/internal/sparky.cliCommit=$(strip $(COMMIT))
endif
LDFLAGS := $(GOLDFLAGS) $(strip $(VERSION_LDFLAGS))

.PHONY: build installer crossbuild menubar menubar-headless package-menubar \
	release-linux release-macos \
	test race vet fmt fmt-check test-python-reference clean

# Always invoke the Go build: dist/ artifacts are not tracked as target
# prerequisites, so a stale binary is never silently reused. Go's build cache
# makes repeated invocation cheap.
build:
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(DIST)/sparky ./cmd/sparky
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(DIST)/sparky-install ./cmd/sparky-install

installer:
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(DIST)/sparky-install ./cmd/sparky-install

# Standalone native binaries for the Mac and both Sparks. Any compiler
# failure fails the whole target; later successes never mask earlier ones.
crossbuild:
	set -eu; \
	for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		for cmdname in sparky sparky-install; do \
			echo "BUILD $$os/$$arch $$cmdname"; \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build $(GOFLAGS) \
				-ldflags '$(LDFLAGS)' -o $(DIST)/$$os-$$arch/$$cmdname ./cmd/$$cmdname || exit 1; \
		done; \
	done

# Native macOS menu bar app. Only the GUI uses cgo; the CLI/install builds
# above stay CGO_ENABLED=0. The .app bundles a standalone copy of the CLI.
menubar:
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(DIST)/sparky ./cmd/sparky
	CGO_ENABLED=1 GOOS=darwin $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(DIST)/sparky-menubar ./cmd/sparky-menubar
	sh tools/package-menubar.sh $(DIST)/Sparky.app

# Headless check: the menubar command must still build without cgo, so the
# CLI/install story and non-macOS builds never depend on AppKit.
menubar-headless:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(DIST)/darwin-arm64/sparky-menubar-stub ./cmd/sparky-menubar
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(DIST)/linux-arm64/sparky-menubar-stub ./cmd/sparky-menubar

# Rebuild only the .app bundle from existing artifacts (reproducible).
package-menubar:
	sh tools/package-menubar.sh $(DIST)/Sparky.app

# Published artifacts for one platform: versioned CLI + installer archives
# (plus the Sparky.app zip on macOS) with per-platform SHA256SUMS under
# $(DIST)/release. These targets are what the release workflow runs, so a
# release is reproducible with ordinary local commands:
#
#   make release-linux VERSION=0.6.3 COMMIT=<40-hex>   # any Go host
#   make release-macos VERSION=0.6.3 COMMIT=<40-hex>   # native macOS arm64
#
# VERSION must be plain numeric X.Y.Z and COMMIT a full 40-hex hash;
# tools/release-bundle.sh refuses anything else rather than mislabelling an
# artifact. The macOS target needs a native macOS runner: the app uses cgo and
# AppKit, so it is never faked headless.
release-linux:
	$(MAKE) crossbuild PLATFORMS=linux/arm64 VERSION='$(VERSION)' COMMIT='$(COMMIT)'
	sh tools/release-bundle.sh --platform linux/arm64 \
		--version '$(VERSION)' --commit '$(COMMIT)' --dist '$(DIST)'

release-macos:
	$(MAKE) crossbuild PLATFORMS=darwin/arm64 VERSION='$(VERSION)' COMMIT='$(COMMIT)'
	$(MAKE) menubar VERSION='$(VERSION)' COMMIT='$(COMMIT)'
	sh tools/release-bundle.sh --platform darwin/arm64 \
		--version '$(VERSION)' --commit '$(COMMIT)' --dist '$(DIST)' \
		--app '$(DIST)/Sparky.app'

test:
	$(GO) test ./...

race:
	$(GO) test -race -count=1 -timeout=$(RACE_TIMEOUT) ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w cmd internal

# CI gate: report unformatted files without rewriting them.
fmt-check:
	@set -eu; files=$$(gofmt -l cmd internal); \
	if [ -n "$$files" ]; then \
		echo "these files are not gofmt-clean:"; echo "$$files"; exit 1; \
	fi

# Reference only: run the original Python tests against the legacy source tree.
# This is for rollback comparison; production is the Go build above.
test-python-reference:
	PYTHONPATH=src python3 -m unittest discover -s tests

clean:
	rm -rf dist
