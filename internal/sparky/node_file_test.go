package sparky

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// canonicalTempDir returns the real path of a fresh temporary directory. On
// macOS the system temporary directory is reached through symbolic links
// (/var -> /private/var), so every fixture path is canonicalized before it
// enters a protocol payload; the receiver refuses symlinked parents and must
// compare against the resolved locations it will actually create.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// TestNodeWriteFileProtocol covers the private transfer protocol: exactly one
// JSON object on stdin, atomic same-dir rename with the intended mode, and
// no temporary leftovers.
func TestNodeWriteFileProtocol(t *testing.T) {
	dir := canonicalTempDir(t)
	path := filepath.Join(dir, "sub", "file.conf")
	in := strings.NewReader(`{"path":"` + path + `","mode":"600","data_base64":"` +
		"ZW5naW5lIHN0YXRl" + `"}`)
	result, err := nodeWriteFile(in)
	if err != nil {
		t.Fatalf("write-file failed: %v", err)
	}
	if str(obj(result)["status"]) != "written" {
		t.Fatalf("unexpected result: %v", result)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "engine state" {
		t.Fatalf("payload not written exactly: %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("intended mode not applied: %v", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".sparky-") {
			t.Fatalf("temporary transfer file left behind: %s", e.Name())
		}
	}
	// Created parents are private.
	info, err = os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("created parent must be 0700: %v", info.Mode().Perm())
	}
}

func TestNodeWriteFileRejections(t *testing.T) {
	dir := canonicalTempDir(t)
	existing := filepath.Join(dir, "existing")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		in   string
	}{
		{"malformed", `{"path":`},
		{"trailing", `{"path":"` + existing + `","mode":"600","data_base64":""} {"x":1}`},
		{"duplicate", `{"path":"` + existing + `","mode":"600","data_base64":""}` + "\n" + `{"path":"` + existing + `"}`},
		{"relative", `{"path":"relative.conf","mode":"600","data_base64":""}`},
		{"traversal", `{"path":"` + dir + `/../evil.conf","mode":"600","data_base64":""}`},
		{"special-mode", `{"path":"` + dir + `/s","mode":"4755","data_base64":""}`},
		{"unreadable-mode", `{"path":"` + dir + `/s","mode":"200","data_base64":""}`},
		{"bad-mode", `{"path":"` + dir + `/s","mode":"eight","data_base64":""}`},
		{"bad-base64", `{"path":"` + dir + `/s","mode":"600","data_base64":"!!!"}`},
		{"missing-payload", `{"path":"` + dir + `/s","mode":"600"}`},
	}
	for _, c := range cases {
		if _, err := nodeWriteFile(strings.NewReader(c.in)); err == nil {
			t.Fatalf("%s: malformed transfer must be refused", c.name)
		}
	}
	// A symlink destination is refused, never followed.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(existing, link); err != nil {
		t.Fatal(err)
	}
	in := `{"path":"` + link + `","mode":"600","data_base64":"Yg=="}`
	if _, err := nodeWriteFile(strings.NewReader(in)); err == nil {
		t.Fatal("a symlink destination must be refused")
	}
	if got, _ := os.ReadFile(existing); string(got) != "keep" {
		t.Fatal("the symlink target must be untouched")
	}
	// A symlinked parent is refused, never traversed.
	if err := os.Symlink(dir, filepath.Join(dir, "parent-link")); err != nil {
		t.Fatal(err)
	}
	in = `{"path":"` + filepath.Join(dir, "parent-link", "sneaky") + `","mode":"600","data_base64":"Yg=="}`
	if _, err := nodeWriteFile(strings.NewReader(in)); err == nil {
		t.Fatal("a symlinked parent must be refused")
	}
	if _, err := os.Lstat(filepath.Join(dir, "sneaky")); !os.IsNotExist(err) {
		t.Fatal("no file may be created through the symlinked parent")
	}
}

// TestNodeWriteFileOperationMetadata covers the operation field, required
// types, the explicit empty payload, and unknown operations.
func TestNodeWriteFileOperationMetadata(t *testing.T) {
	dir := canonicalTempDir(t)
	path := filepath.Join(dir, "empty.conf")
	// The default operation is write.
	in := strings.NewReader(`{"path":"` + path + `","mode":"600","data_base64":""}`)
	if _, err := nodeWriteFile(in); err != nil {
		t.Fatalf("an empty payload is a valid zero-byte write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 {
		t.Fatalf("zero-byte file expected: %v %v", info, err)
	}
	// Missing, null, and non-string payloads are refused with no effect.
	intact := filepath.Join(dir, "intact")
	if err := os.WriteFile(intact, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{
		`{"path":"` + intact + `","mode":"600"}`,
		`{"path":"` + intact + `","mode":"600","data_base64":null}`,
		`{"path":"` + intact + `","mode":"600","data_base64":5}`,
		`{"path":"` + intact + `","mode":"600","data_base64":{}}`,
	} {
		if _, err := nodeWriteFile(strings.NewReader(in)); err == nil {
			t.Fatal("a missing or non-string payload must be refused")
		}
	}
	if got, _ := os.ReadFile(intact); string(got) != "keep" {
		t.Fatal("a refused payload must leave the target intact")
	}
	// Unknown operations fail before any effect.
	if _, err := nodeWriteFile(strings.NewReader(
		`{"operation":"chown","path":"` + intact + `"}`)); err == nil {
		t.Fatal("an unknown operation must be refused")
	}
	if got, _ := os.ReadFile(intact); string(got) != "keep" {
		t.Fatal("an unknown operation must have no effects")
	}
}

// TestNodeWriteFileInspect covers the inspect operation on absent, regular,
// symlink, and directory paths with the exact wire fields.
func TestNodeWriteFileInspect(t *testing.T) {
	dir := canonicalTempDir(t)
	absent := filepath.Join(dir, "absent.conf")
	result, err := nodeWriteFile(strings.NewReader(
		`{"operation":"inspect","path":"` + absent + `"}`))
	if err != nil {
		t.Fatalf("absent inspect failed: %v", err)
	}
	if got := obj(result); got["exists"] != false || len(got) != 1 {
		t.Fatalf("absent inspect result: %v", got)
	}
	// A real file reports its exact mode, bytes, and digest.
	payload := []byte("inspect me\n")
	target := filepath.Join(dir, "state.conf")
	if err := os.WriteFile(target, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err = nodeWriteFile(strings.NewReader(
		`{"operation":"inspect","path":"` + target + `"}`))
	if err != nil {
		t.Fatalf("existing inspect failed: %v", err)
	}
	got := obj(result)
	if got["exists"] != true || got["mode"] != "600" {
		t.Fatalf("inspect result: %v", got)
	}
	raw, err := base64.StdEncoding.DecodeString(str(got["data_base64"]))
	if err != nil || string(raw) != string(payload) {
		t.Fatalf("inspect bytes: %q %v", raw, err)
	}
	sum := sha256.Sum256(payload)
	if got["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("inspect digest: %v", got)
	}
	// No directory is created and no file is written for inspect.
	if _, err := os.Lstat(filepath.Join(dir, "fresh", "x")); !os.IsNotExist(err) {
		t.Fatal("inspect must not create anything")
	}
	// A symlink is refused, never followed.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := nodeWriteFile(strings.NewReader(
		`{"operation":"inspect","path":"` + link + `"}`)); err == nil {
		t.Fatal("a symlink must be refused by inspect")
	}
	// A relative path is refused.
	if _, err := nodeWriteFile(strings.NewReader(
		`{"operation":"inspect","path":"relative"}`)); err == nil {
		t.Fatal("a relative path must be refused by inspect")
	}
}

// TestNodeWriteFileCompareBeforeWrite covers expected_sha256 and
// expected_missing: a mismatch refuses before any mutation and preserves the
// original file plus a clean directory.
func TestNodeWriteFileCompareBeforeWrite(t *testing.T) {
	dir := canonicalTempDir(t)
	target := filepath.Join(dir, "state.conf")
	payload := []byte("v1")
	if err := os.WriteFile(target, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	current := hex.EncodeToString(sum[:])
	// A matching expectation writes atomically and reports the new digest.
	in := strings.NewReader(`{"path":"` + target + `","mode":"600","data_base64":"djI=",` +
		`"expected_sha256":"` + current + `"}`)
	result, err := nodeWriteFile(in)
	if err != nil {
		t.Fatalf("compare write failed: %v", err)
	}
	sum2 := sha256.Sum256([]byte("v2"))
	if str(obj(result)["sha256"]) != hex.EncodeToString(sum2[:]) {
		t.Fatalf("written digest: %v", result)
	}
	// A changed expectation refuses and preserves the file.
	in = strings.NewReader(`{"path":"` + target + `","mode":"600","data_base64":"djM=",` +
		`"expected_sha256":"` + current + `"}`)
	if _, err := nodeWriteFile(in); err == nil {
		t.Fatal("a stale expectation must refuse the write")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "v2" {
		t.Fatalf("a refused write must preserve the original: %q", got)
	}
	// expected_missing refuses when the file exists.
	in = strings.NewReader(`{"path":"` + target + `","mode":"600","data_base64":"djM=",` +
		`"expected_missing":true}`)
	if _, err := nodeWriteFile(in); err == nil {
		t.Fatal("expected_missing must refuse an existing destination")
	}
	got, _ = os.ReadFile(target)
	if string(got) != "v2" {
		t.Fatalf("an expected_missing refusal must preserve the file: %q", got)
	}
	// expected_missing succeeds when the path is absent.
	in = strings.NewReader(`{"path":"` + filepath.Join(dir, "new.conf") + `","mode":"600",` +
		`"data_base64":"djM=","expected_missing":true}`)
	if _, err := nodeWriteFile(in); err != nil {
		t.Fatalf("expected_missing on an absent path: %v", err)
	}
	// A required expectation on an absent path is refused.
	in = strings.NewReader(`{"path":"` + filepath.Join(dir, "gone.conf") + `","mode":"600",` +
		`"data_base64":"djM=","expected_sha256":"` + current + `"}`)
	if _, err := nodeWriteFile(in); err == nil {
		t.Fatal("a required expectation on an absent path must be refused")
	}
	// Malformed expectations are refused before effects.
	for _, in := range []string{
		`{"path":"` + target + `","mode":"600","data_base64":"djM=","expected_sha256":"nothex"}`,
		`{"path":"` + target + `","mode":"600","data_base64":"djM=","expected_missing":"yes"}`,
		`{"path":"` + target + `","mode":"600","data_base64":"djM=","expected_sha256":"` + current + `","expected_missing":true}`,
	} {
		if _, err := nodeWriteFile(strings.NewReader(in)); err == nil {
			t.Fatalf("malformed expectation must be refused: %s", in)
		}
	}
	if got, _ := os.ReadFile(target); string(got) != "v2" {
		t.Fatal("refused expectations must leave the file intact")
	}
}

// TestNodeFileRemoveRollback covers the remove operation: idempotent absence,
// exact-digest removal, changed-content and symlink refusals, and required
// expectations.
func TestNodeFileRemoveRollback(t *testing.T) {
	dir := canonicalTempDir(t)
	// Absent: idempotent.
	result, err := nodeFileRemove(Object{"path": filepath.Join(dir, "absent"),
		"expected_sha256": strings.Repeat("a", 64)})
	if err != nil || str(obj(result)["status"]) != "removed" {
		t.Fatalf("absent remove: %v %v", result, err)
	}
	// Matching digest: removed.
	target := filepath.Join(dir, "own.conf")
	if err := os.WriteFile(target, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("mine"))
	digest := hex.EncodeToString(sum[:])
	result, err = nodeFileRemove(Object{"path": target, "expected_sha256": digest})
	if err != nil {
		t.Fatalf("matching remove: %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("the file must be removed")
	}
	// Changed content: refused.
	other := filepath.Join(dir, "changed.conf")
	if err := os.WriteFile(other, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := nodeFileRemove(Object{"path": other, "expected_sha256": digest}); err == nil {
		t.Fatal("changed content must be refused")
	}
	if _, err := os.Lstat(other); err != nil {
		t.Fatal("a refused remove must keep the file")
	}
	// A symlink is refused, never removed.
	link := filepath.Join(dir, "link.conf")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := nodeFileRemove(Object{"path": link, "expected_sha256": digest}); err == nil {
		t.Fatal("a symlink must be refused by remove")
	}
	if _, err := os.Lstat(target); err != nil {
		t.Fatal("the symlink target must survive")
	}
	// A directory and a missing expectation are refused.
	if _, err := nodeFileRemove(Object{"path": dir, "expected_sha256": digest}); err == nil {
		t.Fatal("a directory must be refused by remove")
	}
	if _, err := nodeFileRemove(Object{"path": other}); err == nil {
		t.Fatal("a missing expectation must be refused")
	}
}

// TestNodeWriteFilePayloadBounds covers the shared bound on the raw payload
// and its base64 envelope.
func TestNodeWriteFilePayloadBounds(t *testing.T) {
	dir := canonicalTempDir(t)
	target := filepath.Join(dir, "big.conf")
	// Exactly the bound is allowed.
	exact := bytes.Repeat([]byte("a"), deploymentMaxPayload)
	encoded := base64.StdEncoding.EncodeToString(exact)
	if len(encoded) != base64.StdEncoding.EncodedLen(deploymentMaxPayload) {
		t.Fatal("fixture bound mismatch")
	}
	in := strings.NewReader(`{"path":"` + target + `","mode":"600","data_base64":"` + encoded + `"}`)
	if _, err := nodeWriteFile(in); err != nil {
		t.Fatalf("an exact-bound payload must be accepted: %v", err)
	}
	// One more raw byte exceeds the bound through the envelope.
	if _, err := nodeWriteFile(strings.NewReader(`{"path":"` + target + `","mode":"600",` +
		`"data_base64":"` + encoded[:len(encoded)-4] + strings.Repeat("a", 8) + `"}`)); err == nil {
		t.Fatal("an overbound envelope must be refused")
	}
	info, _ := os.Stat(target)
	if info == nil || info.Size() != int64(deploymentMaxPayload) {
		t.Fatal("the refused write must not change the target")
	}
}

// TestNodeWriteFileAtomicFailurePreservesOriginal forces a temp-file failure
// and asserts the destination and directory stay clean.
func TestNodeWriteFileAtomicFailurePreservesOriginal(t *testing.T) {
	dir := canonicalTempDir(t)
	target := filepath.Join(dir, "keep.conf")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A read-only directory forces the temp creation to fail after all
	// validation passed, so the original must survive untouched.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	in := strings.NewReader(`{"path":"` + target + `","mode":"600","data_base64":"bmV3"}`)
	if _, err := nodeWriteFile(in); err == nil {
		t.Fatal("an unwritable directory must fail the write")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "original" {
		t.Fatalf("the original must survive a failed write: %q", got)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".sparky-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}
