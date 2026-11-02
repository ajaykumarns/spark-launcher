package sparky

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseNodeFlagsStrict(t *testing.T) {
	if _, _, err := parseNodeFlags([]string{"--bogus", "x"}, map[string]bool{"root": true}, nil, 0); err == nil {
		t.Fatal("unknown flag must fail")
	}
	if _, _, err := parseNodeFlags([]string{"--root"}, map[string]bool{"root": true}, nil, 0); err == nil {
		t.Fatal("missing flag value must fail")
	}
	if _, _, err := parseNodeFlags([]string{"--root", "a", "--root", "b"}, map[string]bool{"root": true}, nil, 0); err == nil {
		t.Fatal("duplicate flag must fail")
	}
	if _, _, err := parseNodeFlags([]string{"junk", "--root", "a"}, map[string]bool{"root": true}, nil, 0); err == nil {
		t.Fatal("unexpected positional must fail")
	}
	flags, positional, err := parseNodeFlags([]string{"--force", "--root", "/x", "plan"},
		map[string]bool{"root": true, "force": true}, map[string]bool{"force": true}, 1)
	if err != nil {
		t.Fatalf("valid flags rejected: %v", err)
	}
	if !flags.set["force"] || flags.values["root"] != "/x" || len(positional) != 1 || positional[0] != "plan" {
		t.Fatalf("parsed flags wrong: %+v %v", flags, positional)
	}
}

func TestNodeMainRejectsBadInvocations(t *testing.T) {
	if _, err := NodeMain(nil, strings.NewReader("")); err == nil {
		t.Fatal("empty args must fail")
	}
	if _, err := NodeMain([]string{"teleport"}, strings.NewReader("")); err == nil {
		t.Fatal("unknown action must fail")
	}
	if _, err := NodeMain([]string{"device", "--extra"}, strings.NewReader("")); err == nil {
		t.Fatal("device must reject flags")
	}
	if _, err := NodeMain([]string{"device", "junk"}, strings.NewReader("")); err == nil {
		t.Fatal("device must reject positional arguments")
	}
	if _, err := NodeMain([]string{"memory"}, strings.NewReader("not json")); err == nil {
		t.Fatal("memory must reject invalid JSON")
	}
	if _, err := NodeMain([]string{"status", "--root", "/tmp", "--model", "m"}, strings.NewReader("")); err == nil {
		t.Fatal("status must not accept --model")
	}
	if _, err := NodeMain([]string{"gateway", "--root", "/tmp", "--force"}, strings.NewReader("")); err == nil {
		t.Fatal("gateway must not accept --force")
	}
	if _, err := NodeMain([]string{"rpc", "--root", "/tmp"}, strings.NewReader("{}")); err == nil {
		t.Fatal("rpc without an operation must fail")
	}
	if _, err := NodeMain([]string{"rpc", "bogus", "--root", "/tmp"}, strings.NewReader("{}")); err == nil {
		t.Fatal("rpc must reject unknown operations")
	}
	if _, err := NodeMain([]string{"rpc", "plan", "extra", "--root", "/tmp"}, strings.NewReader("{}")); err == nil {
		t.Fatal("rpc must reject extra positional arguments")
	}
	if _, err := NodeMain([]string{"supervise", "--model", "m"}, strings.NewReader("")); err == nil {
		t.Fatal("supervise without --file must fail")
	}
	if _, err := NodeMain([]string{"lifecycle", "--file", "/tmp/c.json", "--model", "m", "--action", "explode"}, strings.NewReader("")); err == nil {
		t.Fatal("lifecycle must reject unknown actions")
	}
	if _, err := NodeMain([]string{"load", "--root", "/tmp"}, strings.NewReader("")); err == nil {
		t.Fatal("load without --model must fail")
	}
	if _, err := NodeMain([]string{"render"}, strings.NewReader("")); err == nil {
		t.Fatal("render without --payload must fail")
	}
}

func TestNodePayloadLiteralAndStdin(t *testing.T) {
	payload, err := nodePayload(strings.NewReader(`{"a":1}`), "-")
	if err != nil || num(payload["a"]) != 1 {
		t.Fatalf("stdin payload failed: %v %v", payload, err)
	}
	payload, err = nodePayload(strings.NewReader(""), `{"model":"glm"}`)
	if err != nil || str(payload["model"]) != "glm" {
		t.Fatalf("literal JSON payload failed: %v %v", payload, err)
	}
	if _, err := nodePayload(strings.NewReader(""), "{not json}"); err == nil {
		t.Fatal("invalid literal JSON must fail")
	}
}

func TestNodePayloadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")
	if err := writeObject(path, Object{"nodes": []any{"spark01"}}); err != nil {
		t.Fatal(err)
	}
	payload, err := nodePayloadFile(strings.NewReader(""), path)
	if err != nil || !sameStrings(stringsOf(payload["nodes"]), []string{"spark01"}) {
		t.Fatalf("file payload failed: %v %v", payload, err)
	}
	payload, err = nodePayloadFile(strings.NewReader(`{"stdin":true}`), "-")
	if err != nil || !boolean(payload["stdin"]) {
		t.Fatalf("stdin via file flag failed: %v %v", payload, err)
	}
	if _, err := nodePayloadFile(strings.NewReader(""), filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file must fail")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{oops"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := nodePayloadFile(strings.NewReader(""), bad); err == nil {
		t.Fatal("invalid JSON file must fail")
	}
}
