package sparky

// Build identity for "sparky --version".
//
// These two variables are the only place the published version and commit are
// written for the Go build. A release build overwrites them at link time:
//
//	go build -ldflags "-X sparky/internal/sparky.cliVersion=1.2.3 \
//	                  -X sparky/internal/sparky.cliCommit=<40-hex-sha>" \
//	    -o dist/sparky ./cmd/sparky
//
// They must stay variables, never constants: -X can only rewrite the initial
// value of a package-level string variable, and a message assembled at
// compile time from constants would freeze the development default into the
// release binary. The same reason rules out a package-level const for the
// rendered line; cliVersionLine builds it at call time.
//
// Ordinary development builds (plain "make build") leave both untouched, so
// "sparky --version" keeps printing the bare development version.

import "strings"

// cliVersion is the semantic version reported by "sparky --version". The
// development default tracks the legacy Python package version in
// src/spark_launcher/__init__.py; a release build replaces it with the numeric
// X.Y.Z taken from the release tag.
var cliVersion = "0.6.2"

// cliCommit is the full commit hash the release binary was built from. It is
// empty in development builds, where the working tree may not be any commit
// at all, so the version line stays a bare version there.
var cliCommit = ""

// cliVersionLine renders the single line printed by "sparky --version":
//
//	sparky 1.2.3                      development build (no commit injected)
//	sparky 1.2.3 (commit <40-hex>)    release build
//
// Surrounding whitespace is trimmed because -X values arrive from a shell
// command line, where a stray space or newline is easy to introduce.
func cliVersionLine() string {
	line := "sparky " + strings.TrimSpace(cliVersion)
	if commit := strings.TrimSpace(cliCommit); commit != "" {
		line += " (commit " + commit + ")"
	}
	return line
}

// isReleaseVersion reports whether s is the plain numeric X.Y.Z form a release
// tag must carry, with no leading "v", no prerelease or build suffix and no
// whitespace. tools/release-bundle.sh enforces the same rule before it names
// any artifact, so a mistagged build fails closed instead of shipping a
// mislabelled binary.
func isReleaseVersion(s string) bool {
	if s != strings.TrimSpace(s) {
		return false
	}
	fields := strings.Split(s, ".")
	if len(fields) != 3 {
		return false
	}
	for _, field := range fields {
		if field == "" {
			return false
		}
		for _, r := range field {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}
