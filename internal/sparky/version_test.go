package sparky

// Focused tests for the version line reported by "sparky --version". They
// cover both shapes the CLI can print: the plain development default and the
// release form produced by -ldflags -X injection (see version.go and the
// release workflow). Mutating the package variables is safe here: no test in
// this package runs in parallel, and every case restores the defaults.

import (
	"strings"
	"testing"
)

// fullReleaseCommit is a plausible 40-hex commit for the injected cases.
const fullReleaseCommit = "9f3c1d0a4b6e7c8d5a2f10b3c4d5e6f708192a3b"

// setBuildIdentity points the two link-time variables at the given values for
// one test and restores whatever was there before.
func setBuildIdentity(t *testing.T, version, commit string) {
	t.Helper()
	oldVersion, oldCommit := cliVersion, cliCommit
	t.Cleanup(func() { cliVersion, cliCommit = oldVersion, oldCommit })
	cliVersion, cliCommit = version, commit
}

func TestCLIVersionLineDevelopmentDefault(t *testing.T) {
	// An ordinary "make build" injects nothing, so the compiled-in defaults
	// must print a bare version with no commit field. A test binary that was
	// itself linked with -X is not a development build.
	if strings.TrimSpace(cliCommit) != "" {
		t.Skip("commit injected at link time; development shape does not apply")
	}
	if !isReleaseVersion(cliVersion) {
		t.Fatalf("development default cliVersion %q must be plain numeric X.Y.Z", cliVersion)
	}
	want := "sparky " + cliVersion
	if got := cliVersionLine(); got != want {
		t.Fatalf("development version line = %q, want %q", got, want)
	}
}

func TestCLIVersionLineReleaseBuild(t *testing.T) {
	setBuildIdentity(t, "1.2.3", fullReleaseCommit)
	want := "sparky 1.2.3 (commit " + fullReleaseCommit + ")"
	if got := cliVersionLine(); got != want {
		t.Fatalf("release version line = %q, want %q", got, want)
	}
	if !strings.Contains(cliVersionLine(), fullReleaseCommit) {
		t.Fatal("full commit must be embedded, not abbreviated")
	}
}

func TestCLIVersionLineTrimsInjectedValues(t *testing.T) {
	// -X values come off a shell command line, so tolerate stray whitespace
	// instead of printing it.
	setBuildIdentity(t, " 1.2.3\n", " "+fullReleaseCommit+"\t")
	want := "sparky 1.2.3 (commit " + fullReleaseCommit + ")"
	if got := cliVersionLine(); got != want {
		t.Fatalf("trimmed version line = %q, want %q", got, want)
	}
}

func TestCLIVersionLineIgnoresBlankCommit(t *testing.T) {
	setBuildIdentity(t, "1.2.3", "   ")
	if got, want := cliVersionLine(), "sparky 1.2.3"; got != want {
		t.Fatalf("blank commit must print bare version %q, got %q", want, got)
	}
}

func TestCLIVersionLineUsesInjectedVersion(t *testing.T) {
	// A release tag that disagrees with the development default must win; a
	// stale message assembled at compile time would print the default instead.
	setBuildIdentity(t, "9.9.9", fullReleaseCommit)
	if got, want := cliVersionLine(), "sparky 9.9.9 (commit "+fullReleaseCommit+")"; got != want {
		t.Fatalf("version line = %q, want %q", got, want)
	}
}

func TestIsReleaseVersion(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
	}{
		{"0.6.2", true},
		{"1.2.3", true},
		{"0.0.0", true},
		{"10.20.30", true},
		{"v1.2.3", false},
		{"1.2", false},
		{"1.2.3.4", false},
		{"1.2.3-rc.1", false},
		{"1.2.3+build7", false},
		{"1.2.x", false},
		{"1..3", false},
		{"", false},
		{" 1.2.3", false},
		{"1.2.3 ", false},
		{"-1.2.3", false},
	} {
		if got := isReleaseVersion(tc.value); got != tc.ok {
			t.Errorf("isReleaseVersion(%q) = %v, want %v", tc.value, got, tc.ok)
		}
	}
}

func TestMainVersionPrintsInjectedCommit(t *testing.T) {
	mainDirs(t)
	setBuildIdentity(t, "1.2.3", fullReleaseCommit)
	code, out, errout := runMain([]string{"--version"}, "")
	if code != 0 || errout != "" {
		t.Fatalf("code %d stderr %q", code, errout)
	}
	if out != "sparky 1.2.3 (commit "+fullReleaseCommit+")\n" {
		t.Fatalf("version output = %q", out)
	}
}
