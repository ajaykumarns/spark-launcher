package sparky

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func mustJSON(t *testing.T, s string) Object {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var o Object
	if err := dec.Decode(&o); err != nil {
		t.Fatalf("fixture JSON: %v", err)
	}
	return o
}

func noANSI(s string) bool { return !strings.Contains(s, "\x1b") }

// ---- color precedence ----

func TestColorPrecedence(t *testing.T) {
	cases := []struct {
		opts terminalOptions
		env  map[string]string
		want bool
	}{
		{terminalOptions{Color: "never", TTY: true}, nil, false},
		{terminalOptions{Color: "always", TTY: false}, nil, true},
		{terminalOptions{Color: "auto", TTY: false}, nil, false},
		{terminalOptions{Color: "auto", TTY: true}, nil, true},
		{terminalOptions{Color: "auto", TTY: true}, map[string]string{"NO_COLOR": "1"}, false},
		{terminalOptions{Color: "auto", TTY: true}, map[string]string{"TERM": "dumb"}, false},
		{terminalOptions{Color: "auto", TTY: false}, map[string]string{"FORCE_COLOR": "1"}, true},
		{terminalOptions{Color: "always", TTY: false}, map[string]string{"NO_COLOR": "1"}, true},
		{terminalOptions{Color: "never", TTY: true}, map[string]string{"FORCE_COLOR": "1"}, false},
	}
	for _, c := range cases {
		for _, k := range []string{"NO_COLOR", "TERM", "FORCE_COLOR"} {
			if err := os.Unsetenv(k); err != nil {
				t.Fatalf("unset %s: %v", k, err)
			}
		}
		for k, v := range c.env {
			t.Setenv(k, v)
		}
		out := terminalRender("notice", Object{"title": "state", "message": "running"}, c.opts)
		if c.want && !strings.Contains(out, "\x1b[") {
			t.Errorf("opts %+v env %v: expected ANSI in %q", c.opts, c.env, out)
		}
		if !c.want && !noANSI(out) {
			t.Errorf("opts %+v env %v: unexpected ANSI in %q", c.opts, c.env, out)
		}
	}
}

// ---- sanitization ----

func TestControlInjectionSanitized(t *testing.T) {
	data := Object{"state": "running\x1b[31;1mEVIL", "installed": true}
	out := terminalRender("ocr", data, terminalOptions{Color: "always"})
	if strings.Contains(out, "\x1b[31;1mEVIL") {
		t.Fatalf("injected escape preserved: %q", out)
	}
	if strings.Contains(out, "\x07") {
		t.Fatalf("control char survived: %q", out)
	}
	if !strings.Contains(out, "running [31;1mEVIL") {
		t.Fatalf("content lost: %q", out)
	}
}

// ---- state colors ----

func TestStateStyles(t *testing.T) {
	states := map[string]string{
		"running":  "bright_green",
		"ready":    "bright_green",
		"waiting":  "bright_yellow",
		"failed":   "bright_red",
		"unknown":  "bright_red",
		"stopped":  "dim",
		"disabled": "dim",
		"idle":     "bright_cyan",
	}
	for state, color := range states {
		if got := stateStyle(state); got != color {
			t.Errorf("state %q: got style %q, want %q", state, got, color)
		}
		out := table([]string{"WORKLOAD", "STATE"}, []styledRow{{"m", state}}, "", 80, true)
		if !strings.Contains(out, ansiColors[color]) {
			t.Errorf("state %q: missing color %s in %q", state, color, out)
		}
	}
}

// ---- narrow width keeps every value ----

func statusFixture(t *testing.T) Object {
	return mustJSON(t, `{
		"nodes": {"spark01": {"memory_bytes": {"MemTotal": 68719476736, "MemAvailable": 34359738368}}},
		"memory_usage": {"spark01": {"workloads": [
			{"id": "qwen", "name": "Qwen 235B", "kind": "llm", "state": "running", "context_tokens": 32768, "context_source": "configured", "gpu_bytes": 32212254720, "host_pss_bytes": 2147483648}
		], "warnings": ["Spark01 counters sampled twice."]}},
		"models": {"qwen": {"kind": "llm", "state": "running", "nodes": ["spark01"], "keep_loaded": true}},
		"admission": {"waiting": [{"workload": "whisper"}]}
	}`)
}

func TestStatusNarrowKeepsValues(t *testing.T) {
	out := terminalRender("status", statusFixture(t), terminalOptions{Width: 40, Color: "never"})
	for _, want := range []string{"Qwen 235B", "32,768*", "30.0 GiB", "2.0 GiB", "64.0 GiB", "32.0 GiB", "Waiting: whisper", "Spark01 counters sampled twice.", "Kept loaded: qwen"} {
		if !strings.Contains(out, want) {
			t.Errorf("narrow status missing %q in:\n%s", want, out)
		}
	}
}

func TestStatusWideTable(t *testing.T) {
	out := terminalRender("status", statusFixture(t), terminalOptions{Width: 100, Color: "never"})
	for _, want := range []string{"Running workloads / spark01", "Qwen 235B", "HOST RAM", "32,768*"} {
		if !strings.Contains(out, want) {
			t.Errorf("wide status missing %q in:\n%s", want, out)
		}
	}
}

func TestStatusUnknownReadings(t *testing.T) {
	out := terminalRender("status", mustJSON(t, `{
		"nodes": {"spark02": {"memory_bytes": {}}},
		"memory_usage": {"spark02": {"workloads": [
			{"id": "m", "name": "M", "kind": "llm", "state": "running", "gpu_bytes": null, "host_pss_bytes": null}
		]}}
	}`), terminalOptions{Width: 100, Color: "never"})
	for _, want := range []string{"unknown", "unknown"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing unknown reading in:\n%s", out)
		}
	}
	if strings.Count(out, "unknown") < 3 {
		t.Errorf("expected unknown memory plus unknown context, got:\n%s", out)
	}
}

// ---- speech vs llm distinction ----

func TestSpeechServiceView(t *testing.T) {
	out := terminalRender("speech", mustJSON(t, `{
		"model": "kokoro", "default_voice": "af_heart", "api_base": "http://spark01:8880",
		"nodes": ["spark01"], "device": "cpu", "api_key_required": false,
		"voices": ["af_heart", "bm_george"], "formats": ["wav", "mp3"]
	}`), terminalOptions{Color: "never", Width: 100})
	for _, want := range []string{"Speech generation", "kokoro", "af_heart", "not required", "wav"} {
		if !strings.Contains(out, want) {
			t.Errorf("speech view missing %q in:\n%s", want, out)
		}
	}
}

func TestModelsSeparateFromSpeech(t *testing.T) {
	catalog := mustJSON(t, `{
		"models": [{"id": "garuda/qwen", "context_tokens": 32768, "output_tokens": 4096, "nodes": ["spark01"]}],
		"audio_services": [{"model": "kokoro", "default_voice": "af_heart", "api_base": "http://x", "nodes": ["spark01"]}]
	}`)
	out := terminalRender("models", catalog, terminalOptions{Color: "never", Width: 100})
	if !strings.Contains(out, "Available models") || !strings.Contains(out, "Speech generation") {
		t.Fatalf("models and speech not both shown:\n%s", out)
	}
	if !strings.Contains(out, "qwen") || !strings.Contains(out, "kokoro") {
		t.Fatalf("names missing:\n%s", out)
	}
}

// ---- inventory ----

func TestInventoryKinds(t *testing.T) {
	out := terminalRender("inventory", mustJSON(t, `{
		"models": {
			"qwen": {"kind": "llm", "state": "stopped", "desired": "spark01"},
			"flux": {"kind": "comfy_model", "state": "whatever", "desired": "spark01"}
		}
	}`), terminalOptions{Color: "never", Width: 100})
	if !strings.Contains(out, "LLM") || !strings.Contains(out, "image") {
		t.Fatalf("kinds missing:\n%s", out)
	}
	if !strings.Contains(out, "on demand") {
		t.Errorf("comfy_model should report 'on demand':\n%s", out)
	}
}

// ---- config ----

func TestConfigSummaryAndPlacement(t *testing.T) {
	summary := terminalRender("config", mustJSON(t, `{
		"path": "/etc/sparky/config.yaml",
		"config": {"models": {"qwen": {"kind": "llm", "run_on": "spark01", "keep_loaded": true,
			"settings": {"spark01": {"context_tokens": 32768}}}}}
	}`), terminalOptions{Color: "never", Width: 100})
	if !strings.Contains(summary, "Central model settings") || !strings.Contains(summary, "yes") {
		t.Fatalf("summary missing fields:\n%s", summary)
	}
	placement := terminalRender("config", mustJSON(t, `{
		"model": "qwen",
		"config": {"qwen": {"kind": "llm", "run_on": "spark01", "aliases": ["big"], "keep_loaded": true,
			"settings": {
				"spark01": {"context_tokens": 32768, "concurrency": 8, "enabled": true,
					"parameters": {"max_sequences": 4, "gpu_memory_utilization": 0.9}},
				"both": {"enabled": false, "reason": "insufficient memory"}
			}}}
	}`), terminalOptions{Color: "never", Width: 100})
	for _, want := range []string{"spark01", "both", "disabled", "insufficient memory", "90%"} {
		if !strings.Contains(placement, want) {
			t.Errorf("placement view missing %q in:\n%s", want, placement)
		}
	}
}

// ---- result / devices / launch / logs / doctor / plan ----

func TestResultViews(t *testing.T) {
	out := terminalRender("result", Object{"action": "load", "workload": "qwen", "note": "waited for memory"}, terminalOptions{Color: "never"})
	if !strings.Contains(out, "Model ready") || !strings.Contains(out, "qwen") || !strings.Contains(out, "waited for memory") {
		t.Fatalf("load result wrong:\n%s", out)
	}
	out = terminalRender("result", Object{"action": "placement", "workload": "qwen", "run_on": "both"}, terminalOptions{Color: "never"})
	if !strings.Contains(out, "Placement saved") || !strings.Contains(out, "sparky load qwen") {
		t.Fatalf("placement result wrong:\n%s", out)
	}
}

func TestDevicesUnavailableAndMemory(t *testing.T) {
	out := terminalRender("devices", mustJSON(t, `{
		"devices": {
			"spark01": {"hostname": "spark", "memory_bytes": {"MemTotal": 68719476736, "MemAvailable": 34359738368},
				"disk_bytes": {"free": 1073741824, "total": 2147483648}, "gpu": ["Spark GPU, 41 C, 1000, 8000"],
				"running_containers": ["qwen"], "warnings": ["hot"]},
			"spark02": {"error": "unreachable"}
		}
	}`), terminalOptions{Color: "never", Width: 100})
	for _, want := range []string{"spark01 (spark)", "64.0 GiB", "32.0 GiB", "Spark GPU", "1000 / 8000 MiB", "41 C", "spark02: unavailable", "unreachable", "Device warning", "hot"} {
		if !strings.Contains(out, want) {
			t.Errorf("devices missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "96.0 GiB") {
		t.Errorf("RAM and GPU must never be summed:\n%s", out)
	}
}

func TestLaunchPreview(t *testing.T) {
	out := terminalRender("launch", mustJSON(t, `{
		"model": {"id": "garuda/qwen", "context_tokens": 32768}, "harness": "claude", "gateway": "https://gw",
		"project": "p", "binary": "/bin/x", "generated_files": ["/tmp/a.json"], "extra_arguments": ["--flag", "value"]
	}`), terminalOptions{Color: "never", Width: 100})
	for _, want := range []string{"Launch preview", "garuda/qwen", "32,768 tokens", "/bin/x", "--flag value", "No settings were changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("launch missing %q in:\n%s", want, out)
		}
	}
}

func TestLogsView(t *testing.T) {
	out := terminalRender("logs", mustJSON(t, `{
		"records": [{"started_at": "2026-11-02T10:00:00Z", "harness": "claude", "model": "garuda/qwen",
			"exit_code": 0, "project": "p"}],
		"skipped_files": ["a", "b"]
	}`), terminalOptions{Color: "never", Width: 100})
	if !strings.Contains(out, "Recent harness launches") || !strings.Contains(out, "qwen") || !strings.Contains(out, "ok") {
		t.Fatalf("logs missing fields:\n%s", out)
	}
	if !strings.Contains(out, "Skipped 2 unreadable launch records.") {
		t.Errorf("skipped count missing:\n%s", out)
	}
	empty := terminalRender("logs", Object{"records": []any{}}, terminalOptions{Color: "never"})
	if !strings.Contains(empty, "No local harness launches recorded") {
		t.Fatalf("empty logs view wrong:\n%s", empty)
	}
}

func TestDoctorView(t *testing.T) {
	out := terminalRender("doctor", mustJSON(t, `{
		"checks": [{"name": "gateway", "ok": true, "message": "reachable"}, {"name": "spark01", "ok": false, "message": "timeout"}]
	}`), terminalOptions{Color: "never", Width: 100})
	if !strings.Contains(out, "ok") || !strings.Contains(out, "failed") || !strings.Contains(out, "timeout") {
		t.Fatalf("doctor wrong:\n%s", out)
	}
}

func TestPlanView(t *testing.T) {
	out := terminalRender("plan", mustJSON(t, `{
		"action": "load", "workload": "qwen", "nodes": ["spark01"], "wait_for": ["whisper"],
		"steps": [{"action": "unload", "workload": "whisper", "minimum_available_gib": {"spark01": 10}},
			{"action": "load", "nodes": ["spark01"]}],
		"protected": ["kokoro"]
	}`), terminalOptions{Color: "never", Width: 100})
	for _, want := range []string{"Plan: load qwen on spark01", "whisper", "memory check", "10 GiB available", "Kept loaded: kokoro"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan missing %q in:\n%s", want, out)
		}
	}
}

// ---- OCR ----

func TestOCRView(t *testing.T) {
	out := terminalRender("ocr", Object{"state": "running", "installed": true, "image": "spark-ocr:1.0.0", "running": true, "api_ready": true, "revision": "abc123"}, terminalOptions{Color: "never"})
	for _, want := range []string{"OCR service: running", "Installed: yes", "spark-ocr:1.0.0", "API ready: yes", "abc123"} {
		if !strings.Contains(out, want) {
			t.Errorf("ocr missing %q in:\n%s", want, out)
		}
	}
}

// ---- notice / help ----

func TestNoticeAndHelp(t *testing.T) {
	out := terminalRender("notice", Object{"title": "Warning", "message": "line one\nline two", "tone": "yellow"}, terminalOptions{Color: "never"})
	if !strings.Contains(out, "Warning") || !strings.Contains(out, "line one") || !strings.Contains(out, "line two") {
		t.Fatalf("notice wrong:\n%s", out)
	}
	out = terminalRender("help", Object{"text": "sparky usage:\n  --color always\n  --json"}, terminalOptions{Color: "never"})
	if !strings.Contains(out, "sparky") || !strings.Contains(out, "--color") {
		t.Fatalf("help wrong:\n%s", out)
	}
}

// ---- wrapping, no silent truncation ----

func TestLongValuesNotTruncated(t *testing.T) {
	long := strings.Repeat("word ", 100)
	out := terminalRender("notice", Object{"title": "T", "message": long}, terminalOptions{Color: "never"})
	if got := strings.Count(out, "word"); got != 100 {
		t.Errorf("long value truncated: got %d occurrences, want 100", got)
	}
}

func TestUnknownKindReturnsEmpty(t *testing.T) {
	if out := terminalRender("nonsense", Object{}, terminalOptions{}); out != "" {
		t.Errorf("unknown kind should render empty, got %q", out)
	}
}

// ---- C1 / control sanitization ----

func TestCleanC1AndControls(t *testing.T) {
	out := clean("a\u009bb\x1b]0;osc\x07c\u2028d\t e\x7ff")
	if strings.ContainsAny(out, "\x1b\u009b\u2028\x7f\t") {
		t.Fatalf("control or C1 survived clean: %q", out)
	}
	if out != "a b ]0;osc c d  e f" {
		t.Fatalf("unexpected clean output: %q", out)
	}
	// Cleaned escape payloads are harmless visible text; ESC byte is gone.
	if !utf8.ValidString(out) {
		t.Fatal("clean output is not valid UTF-8")
	}
}

func TestNoticePreservesIntendedLines(t *testing.T) {
	out := terminalRender("notice", Object{"title": "T", "message": "line one\nline two"}, terminalOptions{Color: "never", Width: 100})
	if !strings.Contains(out, "line one") || !strings.Contains(out, "line two") {
		t.Fatalf("line structure lost:\n%s", out)
	}
}

func TestCleanDoesNotMutateCallerData(t *testing.T) {
	data := Object{"state": "running\x1b[31mX", "installed": true}
	_ = terminalRender("ocr", data, terminalOptions{Color: "never"})
	if got := data["state"]; got != "running\x1b[31mX" {
		t.Fatalf("caller data mutated: %q", got)
	}
}

// ---- Unicode-safe wrapping ----

func TestWrapTextUnicodeBoundaries(t *testing.T) {
	text := "తెలుగు భాష భారతదేశం これは日本語のテキストです 载入模型中"
	for _, wrapped := range wrapText(text, 7) {
		if !utf8.ValidString(wrapped) {
			t.Fatalf("invalid UTF-8 line: %q", wrapped)
		}
		if len([]rune(wrapped)) > 7 {
			t.Fatalf("line exceeds width: %q", wrapped)
		}
	}
	joined := strings.Join(wrapText(text, 7), "")
	if strings.ReplaceAll(joined, " ", "") != strings.ReplaceAll(text, " ", "") {
		t.Fatalf("characters lost during wrap:\nwant %q\ngot  %q", text, joined)
	}
}

func TestNarrowStatusUnicodeValuesSurvive(t *testing.T) {
	data := mustJSON(t, `{
		"nodes": {"spark01": {"memory_bytes": {"MemTotal": 68719476736, "MemAvailable": 34359738368}}},
		"memory_usage": {"spark01": {"workloads": [
			{"id": "qwen", "name": "载入模型中 తెలుగు", "kind": "llm", "state": "running", "context_tokens": 32768, "context_source": "configured", "gpu_bytes": 1073741824, "host_pss_bytes": 1073741824}
		]}}
	}`)
	out := terminalRender("status", data, terminalOptions{Width: 30, Color: "never"})
	if !utf8.ValidString(out) {
		t.Fatal("invalid UTF-8 output")
	}
	for _, want := range []string{"载入模型中", "తెలుగు", "32,768*"} {
		if !strings.Contains(out, want) {
			t.Errorf("unicode value lost at width 30: missing %q in:\n%s", want, out)
		}
	}
}

// ---- color propagated to every view ----

func allViewKinds(t *testing.T) map[string]Object {
	t.Helper()
	return map[string]Object{
		"models":    {"models": []any{}},
		"model":     {"id": "m"},
		"speech":    {"model": "kokoro", "api_base": "http://x", "default_voice": "v"},
		"inventory": {"models": Object{}},
		"plan":      {"action": "load", "workload": "m", "nodes": []any{"spark01"}},
		"config":    {"config": Object{}},
		"result":    {"action": "load", "workload": "m"},
		"devices":   {"devices": Object{}},
		"launch":    {"model": Object{}},
		"logs":      {"records": []any{}},
		"doctor":    {"checks": []any{}},
		"status":    {"nodes": Object{}},
		"ocr":       {"state": "running", "installed": true},
		"notice":    {"title": "T", "message": "m"},
		"help":      {"text": "usage"},
	}
}

func TestNoANSIInAnyViewWhenColorNever(t *testing.T) {
	for kind, data := range allViewKinds(t) {
		out := terminalRender(kind, data, terminalOptions{Color: "never", TTY: true, Width: 100})
		if !noANSI(out) {
			t.Errorf("kind %q emitted ANSI with color=never:\n%s", kind, out)
		}
	}
}

func TestANSIInEveryViewWhenColorAlways(t *testing.T) {
	for kind, data := range allViewKinds(t) {
		out := terminalRender(kind, data, terminalOptions{Color: "always", TTY: false, Width: 100})
		if !strings.Contains(out, "\x1b[") {
			t.Errorf("kind %q produced no ANSI with color=always:\n%s", kind, out)
		}
	}
}

// ---- width respected across views ----

func TestNarrowWidthRespected(t *testing.T) {
	cases := map[string]Object{
		"help":   {"text": "sparky usage:\n  --color always sets color mode explicitly for output"},
		"notice": {"title": "Warning", "message": "This warning message is deliberately long so that it must wrap at width twenty."},
		"result": {"action": "load", "workload": "qwen", "note": "The note text is long enough to require wrapping at this narrow width."},
		"ocr":    {"state": "running", "installed": true, "log": "/very/long/installation/log/path/on/spark01/that/wraps.txt"},
	}
	for kind, data := range cases {
		out := terminalRender(kind, data, terminalOptions{Color: "never", Width: 20})
		for _, line := range strings.Split(out, "\n") {
			if visualLen(line) > 20 {
				t.Errorf("kind %q: line of visual length %d exceeds width 20: %q", kind, visualLen(line), line)
			}
		}
	}
}

func TestNarrowNoticeAndResultKeepContent(t *testing.T) {
	out := terminalRender("result", Object{"action": "load", "workload": "qwen", "note": "waited for memory"}, terminalOptions{Color: "never", Width: 20})
	for _, want := range []string{"Model ready", "qwen", "waited for", "memory"} {
		if !strings.Contains(out, want) {
			t.Errorf("narrow result lost %q in:\n%s", want, out)
		}
	}
}

func TestNarrowStatusKeepsLabelsAndValues(t *testing.T) {
	data := statusFixture(t)
	out := terminalRender("status", data, terminalOptions{Width: 20, Color: "never"})
	for _, want := range []string{"Qwen 235B", "32,768*", "30.0 GiB", "2.0 GiB", "Waiting: whisper"} {
		if !strings.Contains(out, want) {
			t.Errorf("narrow status lost %q in:\n%s", want, out)
		}
	}
}

func TestLongOCRPathWrapsInNarrowMode(t *testing.T) {
	out := terminalRender("ocr", Object{"state": "running", "installed": true, "log": "/long/path/segment/one/segment/two/install.log"}, terminalOptions{Color: "never", Width: 40})
	for _, want := range []string{"Installation log on Spark01", "/long/path/", "segment/one", "/segment/tw", "o/install.l", "og"} {
		if !strings.Contains(out, want) {
			t.Fatalf("OCR log path content lost, missing %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if visualLen(line) > 40 {
			t.Errorf("OCR line exceeds width 40: %q", line)
		}
	}
}

func TestUnknownReadingsNeverSummed(t *testing.T) {
	data := mustJSON(t, `{
		"nodes": {"spark02": {"memory_bytes": {"MemTotal": 68719476736}}},
		"memory_usage": {"spark02": {"workloads": [
			{"id": "m", "name": "M", "kind": "llm", "state": "running", "gpu_bytes": 1073741824, "host_pss_bytes": null}
		]}}
	}`)
	out := terminalRender("status", data, terminalOptions{Width: 100, Color: "never"})
	if !strings.Contains(out, "unknown") {
		t.Fatalf("unknown readings not preserved:\n%s", out)
	}
	if strings.Contains(out, "65.0 GiB") {
		t.Fatalf("unknown readings were summed:\n%s", out)
	}
}
