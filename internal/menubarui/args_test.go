package menubarui

import (
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	o, err := ParseArgs([]string{"--cli", "/opt/sparky", "--version"})
	if err != nil {
		t.Fatal(err)
	}
	if o.CLIOverride != "/opt/sparky" || !o.Version {
		t.Fatalf("parse = %+v", o)
	}
	if _, err := ParseArgs([]string{"--help"}); err != nil {
		t.Fatal(err)
	}
	if o, _ := ParseArgs([]string{"-h"}); !o.Help {
		t.Fatal("-h must request help")
	}
}

func TestParseArgsErrors(t *testing.T) {
	for _, args := range [][]string{{"--cli"}, {"--cli", ""}, {"--bogus"}, {"extra"}} {
		if _, err := ParseArgs(args); err == nil {
			t.Fatalf("args %v must fail", args)
		}
	}
}

func TestUsageMentionsCLI(t *testing.T) {
	if !strings.Contains(Usage, "--cli PATH") {
		t.Fatal("usage must document --cli")
	}
}
