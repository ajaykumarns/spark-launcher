package sparky

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecodePreservesExactNumbers(t *testing.T) {
	const seed = `{"seed":18446744073709551615,"ratio":0.1,"flag":true,"none":null,"n":3}`
	o, err := decode([]byte(seed))
	if err != nil {
		t.Fatalf("valid object rejected: %v", err)
	}
	seedNum, ok := o["seed"].(json.Number)
	if !ok || seedNum.String() != "18446744073709551615" {
		t.Fatalf("uint64 seed must survive as exact json.Number, got %#v", o["seed"])
	}
	if o["flag"] != true || o["none"] != nil {
		t.Fatalf("booleans and nulls must survive decode: %#v", o)
	}
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if !strings.Contains(string(b), "18446744073709551615") {
		t.Fatalf("marshaled output lost the exact seed: %s", string(b))
	}
	if strings.Contains(string(b), "e+0") || strings.Contains(string(b), "0.1000000000000000055") {
		t.Fatalf("marshaled output resorted to float spelling: %s", string(b))
	}
}

func TestDecodeRejectsTrailingAndNonObjects(t *testing.T) {
	cases := map[string]string{
		"trailing object": `{"a":1} {"b":2}`,
		"trailing json":   `{"a":1} null`,
		"bare array":      `[1,2]`,
		"bare scalar":     `42`,
		"nothing":         ``,
	}
	for name, in := range cases {
		if _, err := decode([]byte(in)); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
	if _, err := decode([]byte("{\"a\":1}\n")); err != nil {
		t.Fatalf("trailing whitespace is not trailing data: %v", err)
	}
}

func TestCopyObjectPreservesNumbersNullsBooleans(t *testing.T) {
	o, err := decode([]byte(`{"seed":18446744073709551615,"ratio":0.1,"none":null,"flag":true,"nested":{"deep":[18446744073709551615,false,null]}}`))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	cp := copyObject(o)
	seed, ok := cp["seed"].(json.Number)
	if !ok || seed.String() != "18446744073709551615" {
		t.Fatalf("copy lost the exact uint64 seed: %#v", cp["seed"])
	}
	nested := obj(cp["nested"])
	deep := arr(nested["deep"])
	if len(deep) != 3 {
		t.Fatalf("nested collection truncated: %#v", nested)
	}
	if d, ok := deep[0].(json.Number); !ok || d.String() != "18446744073709551615" {
		t.Fatalf("nested seed lost: %#v", deep[0])
	}
	if deep[1] != false || deep[2] != nil {
		t.Fatalf("nested boolean/null lost: %#v", deep)
	}
	// The copy must be independent of the original.
	nested["deep"] = nil
	if len(arr(obj(o["nested"])["deep"])) != 3 {
		t.Fatal("copy is not deep; the original was mutated")
	}
}

func TestNumericHelpersRejectInvalidAndExtremeValues(t *testing.T) {
	if !integer(json.Number("18446744073709551615")) {
		t.Fatal("uint64 json.Number must count as an integer")
	}
	if !integer(json.Number("18446744073709551615")) || !integer(3) || !integer(float64(3)) {
		t.Fatal("integer helper lost valid integers")
	}
	if integer(1.5) || integer(true) || integer("3") || integer(nil) {
		t.Fatal("integer helper accepted a non-integer")
	}
	if integer(json.Number("1e3")) {
		t.Fatal("exponent spelling is not an exact integer budget")
	}
	bad := json.Number("not-a-number")
	if n := num(bad); !math.IsNaN(n) {
		t.Fatalf("invalid json.Number must not become a valid zero, got %v", n)
	}
	if finite(bad) || finite(json.Number("1e999")) || finite(math.NaN()) || finite(math.Inf(1)) || finite(true) || finite("1") {
		t.Fatal("finite helper accepted a non-finite or non-numeric value")
	}
	if !finite(json.Number("18446744073709551615")) || !finite(1.5) || !finite(uint64(9)) {
		t.Fatal("finite helper lost valid finite numbers")
	}
	if num(uint64(7)) != 7 || num(int64(-2)) != -2 || num("x") != 0 {
		t.Fatal("num helper mishandled deliberate numeric Go types")
	}
}

func TestArrSupportsTypedStringCollections(t *testing.T) {
	got := arr([]string{"a", "b"})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("typed string slice lost: %#v", got)
	}
	if arr(nil) != nil || arr(3) != nil {
		t.Fatal("non-collections must stay nil")
	}
	if stringsOf([]string{"x", "y"})[1] != "y" {
		t.Fatal("stringsOf lost a typed slice")
	}
}

func TestCopyValuePreservesNilAndTypedKeys(t *testing.T) {
	src := Object{
		"nilMap":   Object(nil),
		"nilSlice": []any(nil),
		"meta":     map[any]any{7: "seven", "s": Object{"x": 1}},
		"rows":     []Object{{"a": 1}, {"b": 2}},
	}
	cp := copyObject(src)
	if m, ok := cp["nilMap"]; !ok || m != nil {
		t.Fatalf("typed nil map must stay nil, got %#v", m)
	}
	if s, ok := cp["nilSlice"]; !ok || s != nil {
		t.Fatalf("typed nil slice must stay nil, got %#v", s)
	}
	meta, ok := cp["meta"].(map[any]any)
	if !ok {
		t.Fatalf("non-string YAML keys must not be truncated into a string map: %#v", cp["meta"])
	}
	if len(meta) != 2 || meta[7] != "seven" {
		t.Fatalf("numeric metadata keys must survive: %#v", meta)
	}
	// Independence both ways.
	meta["s"] = "changed"
	if inner, ok := src["meta"].(map[any]any)["s"].(Object); !ok || inner["x"] != 1 {
		t.Fatal("copy is not deep; the original metadata was mutated")
	}
	rows := cp["rows"].([]Object)
	rows[0]["a"] = 99
	if src["rows"].([]Object)[0]["a"] != 1 {
		t.Fatal("typed object slices must be copied, not aliased")
	}
}

func TestWriteObjectFailsCleanWithoutFilesystemEffects(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{"good":true}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := writeObject(path, Object{"bad": math.NaN()}); err == nil {
		t.Fatal("NaN content must fail encoding")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("a failed write must not touch the existing file: %q", string(after))
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Fatalf("no temp file may be left behind: %s", e.Name())
		}
	}
	if err := writeObject(filepath.Join(dir, "new.json"), Object{"bad": math.NaN()}); err == nil {
		t.Fatal("encoding failure must be reported")
	}
	if _, err := os.Stat(filepath.Join(dir, "new.json")); !os.IsNotExist(err) {
		t.Fatal("no new file may be created after an encoding failure")
	}
}

func TestIntTextAndToIntPreserveExactIntegers(t *testing.T) {
	if intText(json.Number("18446744073709551615")) != "18446744073709551615" {
		t.Fatal("intText must preserve exact json.Number spelling")
	}
	if intText(int64(-42)) != "-42" || intText(uint64(7)) != "7" {
		t.Fatal("intText lost typed integer values")
	}
	n, err := toInt(json.Number("4096"))
	if err != nil || n != 4096 {
		t.Fatalf("toInt lost a normal integer: %d %v", n, err)
	}
	if _, err := toInt(json.Number("18446744073709551615")); err == nil {
		t.Fatal("machine int conversion must reject uint64 values beyond MaxInt64")
	}
	if _, err := toInt(json.Number("not-a-number")); err == nil {
		t.Fatal("malformed numbers must not convert")
	}
	if _, err := toInt(1.5); err == nil {
		t.Fatal("non-integers must not convert")
	}
	if n, err := toInt(json.Number("18446744073709551615")); err == nil {
		_ = n
	}
	if n, _ := toInt(int64(9223372036854775807)); n != 9223372036854775807 {
		t.Fatal("MaxInt64 must convert exactly")
	}
	if durationSeconds(json.Number("18446744073709551615"), 30) != 30*time.Second {
		t.Fatal("out-of-bounds durations must fall back to the default, not overflow")
	}
	if durationSeconds(json.Number("90"), 30) != 90*time.Second {
		t.Fatal("exact integer durations must be honored")
	}
}

func TestReadObjectAndWriteObjectRoundTripExactNumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	o := Object{"seed": json.Number("18446744073709551615"), "flag": true, "none": nil}
	if err := writeObject(path, o); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	back, err := readObject(path)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if s, ok := back["seed"].(json.Number); !ok || s.String() != "18446744073709551615" {
		t.Fatalf("file roundtrip lost the exact seed: %#v", back["seed"])
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state files must not be world-readable: %v", info)
	}
}
