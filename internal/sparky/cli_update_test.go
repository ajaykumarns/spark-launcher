package sparky

// CLI-surface tests for "sparky update": strict grammar, exit-code
// contract, JSON shape, dispatch, and the private-node self-update exit
// rule. Everything runs offline through the update_test.go harness.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestUpdateGrammarRejectsUnknownOptionsAndPositionals(t *testing.T) {
	mainDirs(t)
	writeMainConfig(t, "https://gw.example")
	for _, args := range [][]string{
		{"update", "--force"},
		{"update", "--yes"},
		{"update", "--repo", "x/y"},
		{"update", "spark01"},
		{"update", "now"},
		{"update", "--tag"},
	} {
		code, _, _ := runMain(args, "")
		if code != 2 {
			t.Fatalf("%v: want usage exit 2, got %d", args, code)
		}
	}
	// Valid grammar parses.
	if _, err := cliParse([]string{"update", "--check", "--tag", "v0.6.5", "--json"}); err != nil {
		t.Fatalf("valid grammar rejected: %v", err)
	}
}

func TestUpdateTagSyntaxFailsBeforeAnyIO(t *testing.T) {
	h := newUpdateHarness(t)
	h.install()
	var out, errout bytes.Buffer
	inv, err := cliParse([]string{"update", "--tag", "0.6.5"})
	if err != nil {
		t.Fatal(err)
	}
	if code := cliUpdateCommand(context.Background(), updateHarnessConfig(), inv, &out, &errout); code != 2 {
		t.Fatalf("bad --tag: code %d", code)
	}
	if _, err := cliParse([]string{"update", "--tag", "not-a-tag"}); err != nil {
		// Grammar accepts the word; updateValidateTag rejects it.
		t.Fatalf("parser should not decide tag syntax: %v", err)
	}
	if len(h.calls) != 0 || h.hasEvent("resolve") {
		t.Fatalf("tag validation must precede all I/O: %v %v", h.calls, h.order)
	}
}

func TestUpdateCheckJSONSingleObjectAndExitZero(t *testing.T) {
	mainDirs(t)
	writeMainConfig(t, "https://gw.example")
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.install()
	code, out, _ := runMain([]string{"update", "--check", "--json"}, "")
	if code != 0 {
		t.Fatalf("check exit: %d", code)
	}
	var report Object
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("stdout must be exactly one JSON object: %v (%q)", err, out)
	}
	if !boolean(report["ok"]) || str(report["action"]) != "check" || str(report["tag"]) != updateTestTag {
		t.Fatalf("report: %+v", report)
	}
	if comps := arr(report["components"]); len(comps) != 3 {
		t.Fatalf("three components: %+v", report)
	}
}

func TestUpdateApplyFailureExitsOneWithJSONReport(t *testing.T) {
	mainDirs(t)
	writeMainConfig(t, "https://gw.example")
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.deviceStatus = "failed"
	h.install()
	code, out, _ := runMain([]string{"update", "--json"}, "")
	if code != 1 {
		t.Fatalf("failure exit: %d", code)
	}
	var report Object
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("partial report must still be one JSON object: %v", err)
	}
	if boolean(report["ok"]) {
		t.Fatalf("ok must be false")
	}
	c := map[string]Object{}
	for _, raw := range arr(report["components"]) {
		o := obj(raw)
		c[str(o["component"])] = o
	}
	if str(c["spark01"]["status"]) != "failed" || str(c["mac"]["status"]) != "pending" {
		t.Fatalf("partial per-target state: %+v", c)
	}
}

func TestUpdateHumanOutputListsAllComponents(t *testing.T) {
	mainDirs(t)
	writeMainConfig(t, "https://gw.example")
	h := newUpdateHarness(t)
	h.makeCurrent()
	h.install()
	code, out, _ := runMain([]string{"update"}, "")
	if code != 0 {
		t.Fatalf("all-current exit: %d", code)
	}
	for _, want := range []string{"mac", "spark01", "spark02", "current", updateTestTag} {
		if !strings.Contains(out, want) {
			t.Fatalf("human output missing %q: %q", want, out)
		}
	}
}

func TestUpdateHelpListsCommand(t *testing.T) {
	mainDirs(t)
	_, out, _ := runMain([]string{"--help"}, "")
	if !strings.Contains(out, "update") || !strings.Contains(out, "sparky update --check") {
		t.Fatalf("help must document update: %q", out)
	}
}

func TestUpdateNodeSelfUpdateErrorReportExitsOne(t *testing.T) {
	mainDirs(t)
	orig := NodeDispatch
	defer func() { NodeDispatch = orig }()
	NodeDispatch = func(args []string, in io.Reader) (any, error) {
		if len(args) == 0 || args[0] != "self-update" {
			t.Errorf("expected self-update words, got %v", args)
		}
		return Object{"action": "self-update", "status": "error", "error": "boom"}, nil
	}
	code, out, _ := runMain([]string{"node", "self-update", "--root", "/srv", "--payload", "-"}, "")
	if code != 1 {
		t.Fatalf("self-update error report must exit 1, got %d", code)
	}
	body, err := decode([]byte(out))
	if err != nil || str(body["status"]) != "error" || str(body["error"]) != "boom" || str(body["action"]) != "self-update" {
		t.Fatalf("report must be the only stdout payload: %q %v", out, err)
	}
}

func TestUpdateNodeOtherCommandsKeepSuccessContract(t *testing.T) {
	mainDirs(t)
	orig := NodeDispatch
	defer func() { NodeDispatch = orig }()
	NodeDispatch = func(args []string, in io.Reader) (any, error) {
		// An identical status:error body under any OTHER node action keeps
		// the existing success contract: the exit rule is self-update-only.
		return Object{"status": "error"}, nil
	}
	code, out, _ := runMain([]string{"node", "status", "--root", "/srv"}, "")
	if code != 0 {
		t.Fatalf("non-self-update node commands must keep exit 0: %d %q", code, out)
	}
	body, err := decode([]byte(out))
	if err != nil || str(body["status"]) != "error" {
		t.Fatalf("value must pass through unchanged: %q %v", out, err)
	}
}
