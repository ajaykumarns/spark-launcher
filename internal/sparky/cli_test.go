package sparky

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func mustParse(t *testing.T, args []string) *cliInvocation {
	t.Helper()
	inv, err := cliParse(args)
	if err != nil {
		t.Fatalf("cliParse(%q) unexpected error: %v", args, err)
	}
	return inv
}

func wantUsage(t *testing.T, args []string, fragment string) *cliInvocation {
	t.Helper()
	inv, err := cliParse(args)
	if err == nil {
		t.Fatalf("cliParse(%q) = %+v, want usage error", args, inv)
	}
	var usage *usageError
	if !errors.As(err, &usage) {
		t.Fatalf("cliParse(%q) error %v is not a usage error", args, err)
	}
	if usage.ExitCode() != 2 {
		t.Fatalf("usage exit code = %d, want 2", usage.ExitCode())
	}
	if fragment != "" && !strings.Contains(err.Error(), fragment) {
		t.Fatalf("error %q does not mention %q", err.Error(), fragment)
	}
	return nil
}

func TestParseNoArgsHelpVersion(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		version bool
		help    bool
	}{
		{nil, false, true},
		{[]string{"--help"}, false, true},
		{[]string{"-h"}, false, true},
		{[]string{"--version"}, true, false},
		{[]string{"--json", "--help"}, false, true},
	} {
		inv := mustParse(t, tc.args)
		if inv.Command != "" {
			t.Fatalf("%q: command = %q, want empty", tc.args, inv.Command)
		}
		if inv.Help != tc.help || inv.Version != tc.version {
			t.Fatalf("%q: help=%v version=%v", tc.args, inv.Help, inv.Version)
		}
	}
}

func TestParseCommandFamilies(t *testing.T) {
	inv := mustParse(t, []string{"status"})
	if inv.Command != "status" || len(inv.Positional) != 0 {
		t.Fatalf("status parse wrong: %+v", inv)
	}

	inv = mustParse(t, []string{"models", "--offline", "glm"})
	if inv.Command != "models" || !reflect.DeepEqual(inv.Positional, []string{"glm"}) ||
		!inv.Flags["offline"] || inv.Flags["installed"] {
		t.Fatalf("models parse wrong: %+v", inv)
	}

	inv = mustParse(t, []string{"ocr", "install", "--repo", "https://x", "--ref", "main", "--rebuild"})
	if inv.Sub != "install" || inv.Values["repo"] != "https://x" || inv.Values["ref"] != "main" || !inv.Flags["rebuild"] {
		t.Fatalf("ocr parse wrong: %+v", inv)
	}

	inv = mustParse(t, []string{"plan", "load", "glm", "--on", "both"})
	if inv.Sub != "" || !reflect.DeepEqual(inv.Positional, []string{"load", "glm"}) || inv.Values["on"] != "both" {
		t.Fatalf("plan parse wrong: %+v", inv)
	}

	inv = mustParse(t, []string{"image-profile", "shared", "--dry-run"})
	if !reflect.DeepEqual(inv.Positional, []string{"shared"}) || !inv.Flags["dry-run"] {
		t.Fatalf("image-profile parse wrong: %+v", inv)
	}
	inv = mustParse(t, []string{"image-profile"})
	if inv.Values["mode"] != "show" {
		t.Fatalf("image-profile default mode = %q", inv.Values["mode"])
	}

	inv = mustParse(t, []string{"config", "show", "glm"})
	if inv.Sub != "show" || !reflect.DeepEqual(inv.Positional, []string{"show", "glm"}) {
		t.Fatalf("config show parse wrong: %+v", inv)
	}
	inv = mustParse(t, []string{"config", "placement", "glm", "spark02"})
	if !reflect.DeepEqual(inv.Positional, []string{"placement", "glm", "spark02"}) {
		t.Fatalf("config placement parse wrong: %+v", inv)
	}

	inv = mustParse(t, []string{"run", "comfyui", "w.json", "--on", "spark02"})
	if !reflect.DeepEqual(inv.Positional, []string{"comfyui", "w.json"}) || inv.Values["on"] != "spark02" {
		t.Fatalf("run parse wrong: %+v", inv)
	}

	inv = mustParse(t, []string{"load", "glm", "--force"})
	if !reflect.DeepEqual(inv.Positional, []string{"glm"}) || !inv.Flags["force"] {
		t.Fatalf("load parse wrong: %+v", inv)
	}

	inv = mustParse(t, []string{"logs", "--limit", "3"})
	if inv.Limit != 3 {
		t.Fatalf("logs limit = %d", inv.Limit)
	}
	inv = mustParse(t, []string{"logs"})
	if inv.Limit != 10 {
		t.Fatalf("logs default limit = %d", inv.Limit)
	}

	inv = mustParse(t, []string{"doctor", "claude"})
	if !reflect.DeepEqual(inv.Positional, []string{"claude"}) {
		t.Fatalf("doctor parse wrong: %+v", inv)
	}

	inv = mustParse(t, []string{"admin", "ssh", "spark02"})
	if inv.Sub != "ssh" || !reflect.DeepEqual(inv.Positional, []string{"ssh", "spark02"}) {
		t.Fatalf("admin ssh parse wrong: %+v", inv)
	}

	inv = mustParse(t, []string{"init", "--gateway", "https://spark:8443", "--prefix", "g/"})
	if inv.Values["gateway"] != "https://spark:8443" || inv.Values["prefix"] != "g/" ||
		inv.Values["worker-host"] != "spark02" || inv.Flags["gateway"] != true {
		t.Fatalf("init parse wrong: %+v", inv)
	}
}

func TestParseHarnessAndDelimiter(t *testing.T) {
	inv := mustParse(t, []string{"claude", "--model", "qwen", "--", "--continue", "--", "--model", "prompt"})
	if inv.Command != "claude" || inv.Values["model"] != "qwen" {
		t.Fatalf("harness parse wrong: %+v", inv)
	}
	if !reflect.DeepEqual(inv.Native, []string{"--continue", "--", "--model", "prompt"}) {
		t.Fatalf("native args wrong: %v", inv.Native)
	}
	if inv.Flags["model-prompt"] {
		t.Fatal("native words leaked into flags")
	}

	// Second -- stays a literal prompt delimiter inside native args.
	inv = mustParse(t, []string{"hermes", "--", "fix", "--", "the bug"})
	if !reflect.DeepEqual(inv.Native, []string{"fix", "--", "the bug"}) {
		t.Fatalf("prompt delimiter lost: %v", inv.Native)
	}

	inv = mustParse(t, []string{"codex", "-m", "glm", "--dry-run"})
	if inv.Values["model"] != "glm" || !inv.Flags["dry-run"] {
		t.Fatalf("-m alias wrong: %+v", inv)
	}

	inv = mustParse(t, []string{"opencode", "--prepare", "--project", "sub"})
	if inv.Values["project"] != "sub" || !inv.Flags["prepare"] {
		t.Fatalf("opencode parse wrong: %+v", inv)
	}

	// A project directory named opencode is not a command.
	inv = mustParse(t, []string{"claude", "--project", "opencode"})
	if inv.Command != "claude" || inv.Values["project"] != "opencode" {
		t.Fatalf("project named opencode misdetected: %+v", inv)
	}
	inv = mustParse(t, []string{"--json", "claude", "--dry-run", "--project", "claude"})
	if inv.Command != "claude" || inv.Values["project"] != "claude" {
		t.Fatalf("project named claude misdetected: %+v", inv)
	}
}

func TestParseGlobalOptions(t *testing.T) {
	// JSON and color before, after, and nested.
	for _, args := range [][]string{
		{"--json", "status"},
		{"status", "--json"},
		{"--json", "ocr", "status"},
		{"ocr", "status", "--json"},
		{"--json", "--color", "always", "devices"},
		{"--color=never", "status", "--json"},
	} {
		inv := mustParse(t, args)
		if !inv.JSON {
			t.Fatalf("%q: JSON not detected", args)
		}
	}
	inv := mustParse(t, []string{"--color", "never", "status", "--color=always"})
	if inv.Color != "always" {
		t.Fatalf("last color should win, got %q", inv.Color)
	}
	inv = mustParse(t, []string{"--color=always", "status", "--color", "never"})
	if inv.Color != "never" {
		t.Fatalf("last nested color should win, got %q", inv.Color)
	}
	if inv := mustParse(t, []string{"status"}); inv.Color != "" {
		t.Fatalf("default color = %q", inv.Color)
	}
	if _, err := cliParse([]string{"--color", "bogus", "status"}); err == nil {
		t.Fatal("invalid --color must be a usage error")
	}
}

func TestParseJSONStopsAtDelimiter(t *testing.T) {
	inv := mustParse(t, []string{"claude", "--", "--json", "--color=always"})
	if inv.JSON || inv.Color != "" {
		t.Fatalf("options after -- must not enable JSON mode: %+v", inv)
	}
	if !reflect.DeepEqual(inv.Native, []string{"--json", "--color=always"}) {
		t.Fatalf("native words changed: %v", inv.Native)
	}
}

func TestParseErrors(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		fragment string
	}{
		{[]string{"statuz"}, "Unknown command: statuz."},
		{[]string{"status", "extra"}, "Unexpected argument: extra"},
		{[]string{"models", "--installed", "--offline"}, "--installed cannot be combined"},
		{[]string{"models", "--installed", "glm"}, "--installed cannot be combined"},
		{[]string{"logs", "--limit", "0"}, "positive whole number"},
		{[]string{"logs", "--limit", "x"}, "positive whole number"},
		{[]string{"plan", "restart", "glm"}, "must be one of load, unload"},
		{[]string{"plan", "load"}, "Missing argument: model"},
		{[]string{"run", "vllm", "w.json"}, "must be one of comfyui"},
		{[]string{"config"}, "Missing argument: action"},
		{[]string{"config", "placement", "glm"}, "Missing argument: on"},
		{[]string{"config", "show", "glm", "extra"}, "Unexpected argument"},
		{[]string{"config", "placement", "glm", "mars"}, "must be one of spark01, spark02, both"},
		{[]string{"admin"}, "Missing argument: action"},
		{[]string{"admin", "ssh", "mars"}, "must be one of spark01, spark02"},
		{[]string{"ocr", "restart"}, "must be one of install, status, start, stop"},
		{[]string{"doctor", "vim"}, "must be one of"},
		{[]string{"status", "--wat"}, "Unknown option: --wat"},
		{[]string{"models", "--offline=yes"}, "does not take a value"},
		{[]string{"init"}, "Option --gateway is required"},
		{[]string{"plan", "load", "glm", "--on", "mars"}, "--on must be one of"},
		{[]string{"--color"}, "needs a value"},
		{[]string{"--color", "loud", "status"}, "--color must be one of"},
		{[]string{"claude", "--dry-run", "--prepare"}, "--dry-run and --prepare cannot be used together"},
		{[]string{"--json", "claude"}, "--json with --dry-run or --prepare"},
	} {
		wantUsage(t, tc.args, tc.fragment)
	}
}

func TestParsePassthroughGuard(t *testing.T) {
	for _, args := range [][]string{
		{"claude", "--", "--model", "x"},
		{"codex", "--", "-c", "model=gpt"},
		{"codex", "--", "--config=model_provider=x"},
	} {
		if _, err := cliParse(args); err == nil {
			t.Fatalf("%q: override not rejected", args)
		} else if _, ok := err.(*usageError); ok {
			t.Fatalf("%q: passthrough rejection is a runtime error, got usage", args)
		}
	}
	// Native unrelated flags and prompt text are allowed.
	inv := mustParse(t, []string{"codex", "--", "-c", "sandbox=read-only", "--", "explain this"})
	if !reflect.DeepEqual(inv.Native, []string{"-c", "sandbox=read-only", "--", "explain this"}) {
		t.Fatalf("native args changed: %v", inv.Native)
	}
}

func TestParseNodeRawWords(t *testing.T) {
	inv := mustParse(t, []string{"node", "rpc", "plan", "--root", "/srv", "--payload", "-"})
	if inv.Command != "node" || !reflect.DeepEqual(inv.NodeArgs, []string{"rpc", "plan", "--root", "/srv", "--payload", "-"}) {
		t.Fatalf("node args wrong: %+v", inv)
	}
	// Global output options are stripped wherever they appear; node flags
	// stay untouched for the dispatcher.
	inv = mustParse(t, []string{"--json", "node", "--color=never", "ocr", "--root", "/srv", "--payload", "-"})
	if !inv.JSON || inv.Color != "never" {
		t.Fatalf("node globals wrong: %+v", inv)
	}
	want := []string{"ocr", "--root", "/srv", "--payload", "-"}
	if !reflect.DeepEqual(inv.NodeArgs, want) {
		t.Fatalf("node args = %v, want %v", inv.NodeArgs, want)
	}
	// Unknown node words are preserved, never parsed by the CLI.
	inv = mustParse(t, []string{"node", "supervise", "--file", "c.json", "--model", "m"})
	if !reflect.DeepEqual(inv.NodeArgs, []string{"supervise", "--file", "c.json", "--model", "m"}) {
		t.Fatalf("node args = %v", inv.NodeArgs)
	}
}

func TestParseColorChoicesValid(t *testing.T) {
	for _, c := range cliColorChoices {
		inv := mustParse(t, []string{"--color", c, "status"})
		if inv.Color != c {
			t.Fatalf("color %q lost", c)
		}
	}
}

func TestNativeArgsSurviveDelimiter(t *testing.T) {
	// Native flags and prompt text after the first -- are kept exactly.
	inv := mustParse(t, []string{"claude", "--model", "qwen", "--", "--continue", "fix it"})
	if inv.Command != "claude" || inv.Values["model"] != "qwen" {
		t.Fatalf("harness parse wrong: %+v", inv)
	}
	if !reflect.DeepEqual(inv.Native, []string{"--continue", "fix it"}) {
		t.Fatalf("native args lost: %v", inv.Native)
	}

	// Non-harness -- ends options; trailing words are still validated
	// positionals, never silently dropped.
	if _, err := cliParse([]string{"status", "--", "extra"}); err == nil {
		t.Fatal("non-harness trailing word must not be dropped")
	} else if _, ok := err.(*usageError); !ok {
		t.Fatalf("expected usage error, got %v", err)
	}
	inv = mustParse(t, []string{"models", "--", "glm"})
	if !reflect.DeepEqual(inv.Positional, []string{"glm"}) {
		t.Fatalf("post-delimiter positional lost: %v", inv.Positional)
	}

	// Node words after an internal delimiter stay verbatim.
	inv = mustParse(t, []string{"node", "render", "--payload", "p", "--", "--model", "x"})
	want := []string{"render", "--payload", "p", "--", "--model", "x"}
	if !reflect.DeepEqual(inv.NodeArgs, want) {
		t.Fatalf("node words after delimiter changed: %v", inv.NodeArgs)
	}
}

func TestInlineValueFlags(t *testing.T) {
	inv := mustParse(t, []string{"init", "--gateway=https://spark:8443"})
	if inv.Values["gateway"] != "https://spark:8443" || !inv.Flags["gateway"] {
		t.Fatalf("inline required flag wrong: %+v", inv)
	}
	inv = mustParse(t, []string{"claude", "-m=qwen"})
	if inv.Values["model"] != "qwen" || !inv.Flags["model"] {
		t.Fatalf("inline -m wrong: %+v", inv)
	}
	inv = mustParse(t, []string{"claude", "-m", "qwen", "--project=.", "--offline"})
	if inv.Values["model"] != "qwen" || inv.Values["project"] != "." || !inv.Flags["offline"] {
		t.Fatalf("mixed forms wrong: %+v", inv)
	}
	// A value token that looks like an option stays a value.
	inv = mustParse(t, []string{"claude", "--project", "--json"})
	if inv.Values["project"] != "--json" || inv.JSON {
		t.Fatalf("value token misread as option: %+v", inv)
	}
}

func TestLogsLimitValidation(t *testing.T) {
	if inv := mustParse(t, []string{"logs"}); inv.Limit != 10 {
		t.Fatalf("default limit = %d", inv.Limit)
	}
	if inv := mustParse(t, []string{"logs", "--limit", "5"}); inv.Limit != 5 {
		t.Fatalf("limit 5 lost")
	}
	for _, bad := range []string{"0", "-3", "x", "", "99999999999999999999"} {
		if _, err := cliParse([]string{"logs", "--limit", bad}); err == nil {
			t.Fatalf("--limit %q accepted", bad)
		} else if code := err.(*usageError).ExitCode(); code != 2 {
			t.Fatalf("--limit %q exit code %d", bad, code)
		}
	}
}

func TestNestedSubcommandGrammar(t *testing.T) {
	inv := mustParse(t, []string{"config", "show", "glm"})
	if !reflect.DeepEqual(inv.Positional, []string{"show", "glm"}) {
		t.Fatalf("config show NAME rejected: %+v", inv)
	}
	wantUsage(t, []string{"config", "validate", "glm"}, "Unexpected argument")
	wantUsage(t, []string{"config", "placement"}, "Missing argument: model")
	wantUsage(t, []string{"config", "placement", "glm"}, "Missing argument: on")
	wantUsage(t, []string{"ocr", "status", "--repo", "r"}, "only valid with 'sparky ocr install'")
	wantUsage(t, []string{"ocr", "status", "--rebuild"}, "only valid with")
	inv = mustParse(t, []string{"ocr", "install", "--repo", "r", "--ref", "v", "--rebuild"})
	if inv.Values["repo"] != "r" || inv.Values["ref"] != "v" || !inv.Flags["rebuild"] {
		t.Fatalf("ocr install flags wrong: %+v", inv)
	}
	inv = mustParse(t, []string{"admin", "ssh", "spark01"})
	if !reflect.DeepEqual(inv.Positional, []string{"ssh", "spark01"}) {
		t.Fatalf("admin ssh target rejected: %+v", inv)
	}
	wantUsage(t, []string{"admin", "enable-harnesses", "spark01"}, "Unexpected argument")
	wantUsage(t, []string{"admin", "ssh", "mars"}, "must be one of spark01, spark02")
}

func TestHelpBeatsMissingRequired(t *testing.T) {
	for _, args := range [][]string{
		{"load", "--help"},
		{"ocr", "--help"},
		{"config", "placement", "--help"},
		{"init", "--help"},
		{"--json", "load", "-h"},
	} {
		inv := mustParse(t, args)
		if !inv.Help {
			t.Fatalf("%q: help not recorded", args)
		}
	}
	// A lone global option is still a usage error.
	inv, err := cliParse([]string{"--json"})
	if err == nil {
		t.Fatalf("--json alone parsed as %+v", inv)
	}
	if code := err.(*usageError).ExitCode(); code != 2 {
		t.Fatalf("--json alone exit code %d", code)
	}
	// --version is a top-level option only.
	if inv := mustParse(t, []string{"--version", "--json"}); !inv.Version || !inv.JSON {
		t.Fatalf("--version parse wrong: %+v", inv)
	}
	wantUsage(t, []string{"status", "--version"}, "Unknown option: --version")
	// "--" before a command cannot introduce one.
	wantUsage(t, []string{"--", "status"}, "Choose a command")
}

func TestColorAndJSONOccurrences(t *testing.T) {
	wantUsage(t, []string{"--color", "loud", "status"}, "--color must be one of")
	wantUsage(t, []string{"--color=loud", "status"}, "--color must be one of")
	wantUsage(t, []string{"status", "--color", "loud"}, "--color must be one of")
	wantUsage(t, []string{"--json=yes", "status"}, "does not take a value")
	wantUsage(t, []string{"node", "--color", "loud", "ocr"}, "--color must be one of")
	wantUsage(t, []string{"node", "--color"}, "needs a value")
	// Tokens after the node action word belong to the dispatcher and
	// pass through verbatim, even when they name output options.
	inv := mustParse(t, []string{"node", "ocr", "--color"})
	if !reflect.DeepEqual(inv.NodeArgs, []string{"ocr", "--color"}) {
		t.Fatalf("node raw words changed: %v", inv.NodeArgs)
	}
	inv = mustParse(t, []string{"--color", "never", "status", "--color=always"})
	if inv.Color != "always" {
		t.Fatalf("last valid color should win, got %q", inv.Color)
	}
	// Node output options are stripped without consuming other values.
	inv = mustParse(t, []string{"node", "--color=never", "rpc", "plan", "--payload", "--json"})
	want := []string{"rpc", "plan", "--payload", "--json"}
	if inv.Color != "never" || !reflect.DeepEqual(inv.NodeArgs, want) {
		t.Fatalf("node strip wrong: color=%q args=%v", inv.Color, inv.NodeArgs)
	}
}

func TestDelimiterPositionalParity(t *testing.T) {
	// The delimiter only changes where words come from; validation happens
	// after the tail is appended, so "--" forms parse identically.
	inv := mustParse(t, []string{"load", "--force", "--", "glm"})
	if !reflect.DeepEqual(inv.Positional, []string{"glm"}) || !inv.Flags["force"] {
		t.Fatalf("load -- glm wrong: %+v", inv)
	}
	if inv2 := mustParse(t, []string{"load", "glm"}); !reflect.DeepEqual(inv2.Positional, inv.Positional) {
		t.Fatalf("load glm differs from load -- glm: %+v", inv2)
	}

	inv = mustParse(t, []string{"plan", "--", "load", "glm"})
	if !reflect.DeepEqual(inv.Positional, []string{"load", "glm"}) {
		t.Fatalf("plan -- load glm wrong: %+v", inv)
	}

	inv = mustParse(t, []string{"config", "--", "placement", "glm", "both"})
	if inv.Sub != "placement" || !reflect.DeepEqual(inv.Positional, []string{"placement", "glm", "both"}) {
		t.Fatalf("config -- placement wrong: %+v", inv)
	}
	if inv2 := mustParse(t, []string{"config", "placement", "glm", "both"}); inv2.Sub != inv.Sub {
		t.Fatalf("Sub not derived after tail parsing: %+v vs %+v", inv, inv2)
	}

	// Extra words after -- are still validated, never dropped.
	wantUsage(t, []string{"status", "--", "extra"}, "Unexpected argument: extra")

	// A delimiter does not turn later option-looking words into options.
	inv = mustParse(t, []string{"models", "--", "--json"})
	if inv.JSON || !reflect.DeepEqual(inv.Positional, []string{"--json"}) {
		t.Fatalf("models -- --json wrong: %+v", inv)
	}
}

func TestNodeValueTokensOpaque(t *testing.T) {
	// A global-looking token that is the value of a node option stays
	// verbatim and does not switch output modes.
	inv := mustParse(t, []string{"node", "render", "--payload", "--json"})
	if inv.JSON {
		t.Fatalf("--json as payload value enabled JSON: %+v", inv)
	}
	if !reflect.DeepEqual(inv.NodeArgs, []string{"render", "--payload", "--json"}) {
		t.Fatalf("payload value changed: %v", inv.NodeArgs)
	}

	inv = mustParse(t, []string{"node", "load", "--model", "--color"})
	if inv.Color != "" {
		t.Fatalf("--color as model value set color: %+v", inv)
	}
	if !reflect.DeepEqual(inv.NodeArgs, []string{"load", "--model", "--color"}) {
		t.Fatalf("model value changed: %v", inv.NodeArgs)
	}

	// Unknown private flags keep their next token too, for dispatcher
	// strict errors.
	inv = mustParse(t, []string{"node", "supervise", "--wat", "--json"})
	if inv.JSON {
		t.Fatalf("--json after unknown flag enabled JSON: %+v", inv)
	}
	if !reflect.DeepEqual(inv.NodeArgs, []string{"supervise", "--wat", "--json"}) {
		t.Fatalf("unknown flag words changed: %v", inv.NodeArgs)
	}

	// Known node booleans do not swallow the next token.
	inv = mustParse(t, []string{"node", "load", "--force", "--json", "--root", "/srv"})
	if !inv.JSON {
		t.Fatalf("real --json not detected: %+v", inv)
	}
	if !reflect.DeepEqual(inv.NodeArgs, []string{"load", "--force", "--root", "/srv"}) {
		t.Fatalf("bool handling wrong: %v", inv.NodeArgs)
	}

	// Top-level --json before node still selects JSON output.
	inv = mustParse(t, []string{"--json", "node", "render", "--payload", "--json"})
	if !inv.JSON {
		t.Fatal("top-level --json lost")
	}
	if !reflect.DeepEqual(inv.NodeArgs, []string{"render", "--payload", "--json"}) {
		t.Fatalf("node args changed: %v", inv.NodeArgs)
	}

	// "--" still ends global stripping; the rest is raw.
	inv = mustParse(t, []string{"node", "render", "--payload", "p", "--", "--json"})
	if inv.JSON {
		t.Fatalf("--json after -- enabled JSON: %+v", inv)
	}
	if !reflect.DeepEqual(inv.NodeArgs, []string{"render", "--payload", "p", "--", "--json"}) {
		t.Fatalf("post-delimiter words changed: %v", inv.NodeArgs)
	}
}

func TestHelpInlineRejected(t *testing.T) {
	for _, args := range [][]string{
		{"claude", "--help=x"},
		{"claude", "-h=x"},
		{"load", "--help=true"},
		{"--help=x"},
		{"-h=x"},
	} {
		wantUsage(t, args, "does not take a value")
	}
}

func TestHelpBypassesHarnessJSONRestriction(t *testing.T) {
	inv := mustParse(t, []string{"--json", "claude", "--help"})
	if !inv.Help || !inv.JSON {
		t.Fatalf("--json claude --help wrong: %+v", inv)
	}
	// Missing required/minimum arguments still yield help.
	inv = mustParse(t, []string{"--json", "load", "-h"})
	if !inv.Help || !inv.JSON {
		t.Fatalf("--json load -h wrong: %+v", inv)
	}
	// But deliberately conflicting input is still rejected.
	wantUsage(t, []string{"claude", "--dry-run", "--prepare", "--help"}, "cannot be used together")
}

func TestProjectNamesNotCommands(t *testing.T) {
	// Values that equal command words stay values; the real command
	// position decides dispatch.
	inv := mustParse(t, []string{"claude", "--project", "opencode"})
	if inv.Command != "claude" || inv.Values["project"] != "opencode" {
		t.Fatalf("project named opencode misdetected: %+v", inv)
	}
	inv = mustParse(t, []string{"--json", "claude", "--dry-run", "--project", "claude"})
	if inv.Command != "claude" || inv.Values["project"] != "claude" || !inv.JSON {
		t.Fatalf("project named claude misdetected: %+v", inv)
	}
	inv = mustParse(t, []string{"claude", "--project", "--version"})
	if inv.Version || inv.Values["project"] != "--version" {
		t.Fatalf("opaque value misread: %+v", inv)
	}
}
