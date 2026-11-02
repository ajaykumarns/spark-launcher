package sparky

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testRefSource = "../.." + string(os.PathSeparator) + "src" + string(os.PathSeparator) + "spark_launcher" + string(os.PathSeparator) + "comfy_guard.py"

func TestComfyGuardScriptByteIdentical(t *testing.T) {
	ref, err := os.ReadFile(testRefSource)
	if err != nil {
		t.Skipf("reference source unavailable: %v", err)
	}
	got := ComfyGuardScript()
	if string(got) != string(ref) {
		t.Fatal("embedded comfy_guard.py is not byte-identical to src/spark_launcher/comfy_guard.py")
	}
	// Defensive copy: mutating the result must not affect the asset.
	got[0] ^= 0xFF
	if string(ComfyGuardScript()) != string(ref) {
		t.Fatal("ComfyGuardScript returned a view, not a copy")
	}
	for _, route := range []string{"/sparky/state", "/sparky/free", "/sparky/drain"} {
		if !strings.Contains(string(ref), route) {
			t.Errorf("asset missing protocol route %s", route)
		}
	}
	if !strings.Contains(string(ref), "SPARKY_COMFY_CONFIG") {
		t.Error("asset missing SPARKY_COMFY_CONFIG env contract")
	}
}

func TestSparkyControllerUnit(t *testing.T) {
	unit, err := SparkyControllerUnit("ajay", "/opt/spark/bin/sparky", "/etc/spark/cluster.json", "/srv/spark")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"[Unit]",
		"Wants=network-online.target",
		"After=network-online.target",
		"[Service]",
		"Type=simple",
		"User=ajay",
		"WorkingDirectory=/srv/spark",
		"EnvironmentFile=/srv/spark/vllm/api.env",
		"ExecStart=:\"/opt/spark/bin/sparky\" --json node controller --file \"/etc/spark/cluster.json\"",
		"Restart=on-failure",
		"RestartSec=3",
		"UMask=0077",
		"[Install]",
		"WantedBy=multi-user.target",
	}
	for _, w := range want {
		if !strings.Contains(unit, w+"\n") {
			t.Errorf("unit missing %q\n%s", w, unit)
		}
	}
	for _, forbidden := range []string{"llama-swap", "garuda-models", "Requires="} {
		if strings.Contains(unit, forbidden) {
			t.Errorf("unit must not contain %q\n%s", forbidden, unit)
		}
	}
	// EnvironmentFile must carry the path only, never values or argv.
	if strings.Contains(unit, "VLLM_API_KEY=") {
		t.Errorf("unit must not embed secret values:\n%s", unit)
	}
}

func TestSparkyControllerUnitQuotedPaths(t *testing.T) {
	unit, err := SparkyControllerUnit("svc", `/opt/a b/sp%ar$1`, `/etc/spark/"cfg".json`, `/srv/ro ot$X`)
	if err != nil {
		t.Fatal(err)
	}
	// Exec lines: colon prefix disables env substitution, so $ stays
	// literal; % is doubled against specifiers; quotes are C-escaped.
	if !strings.Contains(unit, `ExecStart=:"/opt/a b/sp%%ar$1" --json node controller --file "/etc/spark/\"cfg\".json"`) {
		t.Errorf("missing exec-prefix quoting with literal $ and escaped %%:\n%s", unit)
	}
	// WorkingDirectory= is a whole-value directive: systemd's parser keeps
	// quotes literal (verified against systemd-analyze), so the path is
	// rendered unquoted with spaces and $ preserved and % doubled against
	// specifier expansion.
	if !strings.Contains(unit, "WorkingDirectory=/srv/ro ot$X\n") {
		t.Errorf("missing literal space and $ in unquoted WorkingDirectory:\n%s", unit)
	}
	// EnvironmentFile= is a whole-value directive: systemd's parser keeps
	// quotes literal (verified against systemd-analyze: a quoted path is
	// rejected as "not absolute" and silently ignored), so the path is
	// rendered unquoted with spaces and $ preserved and % doubled against
	// specifier expansion.
	if !strings.Contains(unit, "EnvironmentFile=/srv/ro ot$X/vllm/api.env\n") {
		t.Errorf("missing EnvironmentFile path under root:\n%s", unit)
	}
	if strings.Contains(unit, "$$") {
		t.Errorf("no directive may double $ (paths are literal):\n%s", unit)
	}
}

func TestWorkingDirectoryLiteralRendering(t *testing.T) {
	// Regression: the rendered WorkingDirectory must be the exact literal
	// path with NO surrounding quotes; systemd treats quotes as part of the
	// path and fails with "path is not absolute".
	const root = "/home/spark-user/spark-cluster"
	unit, err := SparkyControllerUnit("spark-user", "/opt/spark/bin/sparky", "/srv/config.json", root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit, "WorkingDirectory="+root+"\n") {
		t.Errorf("WorkingDirectory must be the exact unquoted literal path:\n%s", unit)
	}
	// A literal % in the path must be doubled so specifier expansion
	// yields the original path.
	unit, err = SparkyControllerUnit("svc", "/h", "/c", "/srv/100%load")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit, "WorkingDirectory=/srv/100%%load\n") {
		t.Errorf("literal %% must be doubled in WorkingDirectory:\n%s", unit)
	}
	// A path that cannot be represented faithfully unquoted (quote or
	// backslash) must be rejected, not rendered wrong.
	if _, err := SparkyControllerUnit("svc", "/h", "/c", `/srv/r\o`); err == nil {
		t.Error("backslash path accepted for WorkingDirectory")
	}
	if _, err := SparkyControllerUnit("svc", "/h", "/c", `/srv/r"o`); err == nil {
		t.Error("quote path accepted for WorkingDirectory")
	}
}

func TestSparkyModelUnitTemplate(t *testing.T) {
	unit, err := SparkyModelUnitTemplate("svc", "/opt/spark/bin/sparky", "/etc/spark/cluster.json")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ExecStart=:\"/opt/spark/bin/sparky\" --json node supervise --file \"/etc/spark/cluster.json\" --model %i",
		"ExecStop=:\"/opt/spark/bin/sparky\" --json node lifecycle --file \"/etc/spark/cluster.json\" --model %i --action stop",
		"ExecStopPost=:\"/opt/spark/bin/sparky\" --json node lifecycle --file \"/etc/spark/cluster.json\" --model %i --action cleanup",
		"KillMode=mixed",
		"TimeoutStopSec=300",
		"Restart=no",
	}
	for _, w := range want {
		if !strings.Contains(unit, w) {
			t.Errorf("unit missing %q\n%s", w, unit)
		}
	}
	for _, forbidden := range []string{"[Install]", "WantedBy", "Restart=on-failure", "Restart=always"} {
		if strings.Contains(unit, forbidden) {
			t.Errorf("model unit must not contain %q\n%s", forbidden, unit)
		}
	}
	if strings.Count(unit, "%i") != 4 {
		t.Errorf("expected exactly 4 literal %%i specifiers:\n%s", unit)
	}
}

func TestSparkyModelUnitTemplateEscapesPercentInPaths(t *testing.T) {
	unit, err := SparkyModelUnitTemplate("svc", "/o%h/p", "/c%f")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit, `ExecStart=:"/o%%h/p" --json node supervise --file "/c%%f" --model %i`) {
		t.Errorf("user %% not escaped while %%i preserved:\n%s", unit)
	}
}

func TestComfyUIDropin(t *testing.T) {
	dropin, err := ComfyUIDropin("/opt/spark/bin/sparky", "/etc/spark/node.json", "/opt/comfy/custom_nodes/spark_guard.py")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"[Service]",
		"Environment=\"SPARKY_COMFY_CONFIG=/opt/comfy/custom_nodes/spark_guard.py\"",
		"ExecStop=:\"/opt/spark/bin/sparky\" --json node drain-comfy --file \"/etc/spark/node.json\"",
		"TimeoutStopSec=infinity",
	}
	for _, w := range want {
		if !strings.Contains(dropin, w) {
			t.Errorf("dropin missing %q\n%s", w, dropin)
		}
	}
	// The drain must run in ExecStop (before the engine dies), never in
	// ExecStopPost, and the original service user must not be overridden.
	if strings.Contains(dropin, "ExecStopPost=") {
		t.Errorf("dropin must not add ExecStopPost:\n%s", dropin)
	}
	if strings.Contains(dropin, "User=") {
		t.Errorf("dropin must not override User=:\n%s", dropin)
	}
	if !strings.Contains(dropin, "ExecStop=:") {
		t.Errorf("ExecStop must carry the drain command:\n%s", dropin)
	}
	if strings.Contains(dropin, "KillMode") || strings.Contains(dropin, "cgroup") {
		t.Error("dropin must not touch cgroups or kill mode")
	}
}

func TestLegacyModelShim(t *testing.T) {
	shim, err := LegacyModelShim("/opt/spark bin/sparky", "/etc/spark/cluster's.json")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"#!/bin/sh",
		"SPARKY_HELPER='/opt/spark bin/sparky'",
		`SPARKY_CONFIG='/etc/spark/cluster'"'"'s.json'`,
		"exec \"$SPARKY_HELPER\" --json node supervise --file \"$SPARKY_CONFIG\" --model \"$2\"",
		"exec \"$SPARKY_HELPER\" --json node lifecycle --file \"$SPARKY_CONFIG\" --model \"$2\" --action \"$1\"",
	}
	for _, w := range want {
		if !strings.Contains(shim, w) {
			t.Errorf("shim missing %q\n%s", w, shim)
		}
	}
	if !strings.Contains(shim, "exit 2") || !strings.Contains(shim, `{"error":`) {
		t.Error("shim must emit fixed JSON error and exit 2 on bad args")
	}
	if strings.Contains(shim, "python") || strings.Contains(shim, "import ") {
		t.Error("shim must not import Python Sparky")
	}
}

func TestLegacyModelShimArgvBehavior(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "stub-helper")
	config := filepath.Join(dir, "config.json")
	out := filepath.Join(dir, "out")
	if err := os.WriteFile(config, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	shim, err := LegacyModelShim(helper, config)
	if err != nil {
		t.Fatal(err)
	}
	shimPath := filepath.Join(dir, "sparky-model")
	if err := os.WriteFile(shimPath, []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}

	runShim := func(args ...string) (string, error) {
		cmd := exec.Command(shimPath, args...)
		cmd.Env = append(os.Environ(), "SPARKY_STUB_OUT="+out)
		b, err := cmd.CombinedOutput()
		return string(b), err
	}

	// The stub helper must record argv; rewrite the helper to the test
	// binary via env indirection is not possible with exec directly, so the
	// shim's SPARKY_HELPER is the helper path itself: overwrite it with a
	// tiny shell stub.
	stub := "#!/bin/sh\nprintf '%s' \"$*\" > \"$SPARKY_STUB_OUT\"\n"
	if err := os.WriteFile(helper, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := runShim("run", "llama-3"); err != nil {
		t.Fatalf("run: %v", err)
	}
	argv, _ := os.ReadFile(out)
	if got := string(argv); got != "--json node supervise --file "+config+" --model llama-3" {
		t.Errorf("run argv = %q", got)
	}

	if _, err := runShim("stop", "llama-3"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	argv, _ = os.ReadFile(out)
	if got := string(argv); got != "--json node lifecycle --file "+config+" --model llama-3 --action stop" {
		t.Errorf("stop argv = %q", got)
	}

	if _, err := runShim("cleanup", "m1"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	argv, _ = os.ReadFile(out)
	if got := string(argv); got != "--json node lifecycle --file "+config+" --model m1 --action cleanup" {
		t.Errorf("cleanup argv = %q", got)
	}

	// Wrong arg count and unknown action: fixed JSON error, exit 2.
	for _, bad := range [][]string{{"run"}, {"run", "m", "extra"}, {"explode", "m"}} {
		outBytes, err := runShim(bad...)
		if err == nil {
			t.Errorf("args %v: expected exit 2", bad)
			continue
		}
		if !strings.Contains(outBytes, `{"error":`) {
			t.Errorf("args %v: expected JSON error, got %q", bad, outBytes)
		}
	}
}

func TestTemplateControlRejection(t *testing.T) {
	if _, err := SparkyControllerUnit("svc", "helper\nx", "/c", "/r"); err == nil {
		t.Error("newline in helper path accepted")
	}
	if _, err := SparkyControllerUnit("svc", "helper\x00x", "/c", "/r"); err == nil {
		t.Error("NUL in helper path accepted")
	}
	if _, err := SparkyControllerUnit("svc", "helper\x07x", "/c", "/r"); err == nil {
		t.Error("control char in helper path accepted")
	}
	if _, err := SparkyControllerUnit("", "h", "/c", "/r"); err == nil {
		t.Error("empty user accepted")
	}
	if _, err := ComfyUIDropin("h", "/n", "p\x01q"); err == nil {
		t.Error("control char in plugin path accepted")
	}
}

func TestTemplateValidationRejectsInjectionAndRelativePaths(t *testing.T) {
	badUsers := []string{"root x", "root\nx", "root;id", "ro%ot", "ro$ot", "-x", ".x", "x\ty", "x\x7f", "a b", ""}
	for _, u := range badUsers {
		if _, err := SparkyControllerUnit(u, "/h", "/c", "/r"); err == nil {
			t.Errorf("user %q accepted", u)
		}
		if _, err := SparkyModelUnitTemplate(u, "/h", "/c"); err == nil {
			t.Errorf("model unit accepted user %q", u)
		}
	}
	goodUsers := []string{"ajay", "sparky_1", "svc-9.test", "spark$", "1000", "0"}
	for _, u := range goodUsers {
		if _, err := SparkyControllerUnit(u, "/h", "/c", "/r"); err != nil {
			t.Errorf("valid user %q rejected: %v", u, err)
		}
	}
	relative := []string{"rel/helper", "./h", "h", ""}
	for i := range relative {
		h := relative[i]
		if _, err := SparkyControllerUnit("svc", h, "/c", "/r"); err == nil {
			t.Errorf("relative/empty helper %q accepted", h)
		}
		if _, err := SparkyModelUnitTemplate("svc", "/h", h); err == nil {
			t.Errorf("relative/empty config %q accepted", h)
		}
		if _, err := ComfyUIDropin("/h", h, "/p"); err == nil {
			t.Errorf("relative/empty node path %q accepted", h)
		}
		if _, err := LegacyModelShim(h, "/c"); err == nil {
			t.Errorf("relative/empty shim helper %q accepted", h)
		}
	}
	if _, err := SparkyControllerUnit("svc", "/h", "/c", "relative-root"); err == nil {
		t.Error("relative root accepted")
	}
	// Errors must not echo unsafe raw input.
	_, err := SparkyControllerUnit("svc; injected", "/h", "/c", "/r")
	if err != nil && strings.Contains(err.Error(), "svc") {
		t.Errorf("error echoed raw user input: %v", err)
	}
}

func TestEnvironmentValueLiteralDollarPercentAndEndingQuote(t *testing.T) {
	dropin, err := ComfyUIDropin("/h", "/n", `/opt/cfg$d%r"q`)
	if err != nil {
		t.Fatal(err)
	}
	// The WHOLE NAME=value assignment is quoted; a trailing literal quote in
	// the path must be escaped, never trimmed off.
	want := `Environment="SPARKY_COMFY_CONFIG=/opt/cfg$d%%r\"q"`
	if !strings.Contains(dropin, want) {
		t.Errorf("Environment= must quote the whole assignment with literal $ and %%, escaped quotes:\n%s", dropin)
	}
	// WorkingDirectory must keep $ literal (no env expansion there) and
	// must be unquoted; a quote or backslash cannot be represented and is
	// rejected, so use a path without them.
	unit, err := SparkyControllerUnit("svc", "/h", "/c", `/r$d%i`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit, "WorkingDirectory=/r$d%%i\n") {
		t.Errorf("WorkingDirectory must keep $ literal and double %%:\n%s", unit)
	}
	if strings.Contains(unit, "$$") {
		t.Errorf("no $ doubling allowed in path directives:\n%s", unit)
	}
}
