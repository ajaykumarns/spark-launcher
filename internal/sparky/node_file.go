// Private worker-side file protocol: the receiver for the bounded JSON
// transfer metadata, the atomic write, the strict readonly inspect, and the
// own-payload-only remove. This file depends only on common.go and the
// standard library; it never touches a runtime, coordinator, or CLI effect.
package sparky

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// deploymentMaxPayload bounds one raw payload. A unit text, node metadata
// file, or engine script is far below this; anything larger is refused
// before any filesystem or network effect.
const deploymentMaxPayload = 16 << 20

// deploymentMaxMetadata bounds the full transfer metadata envelope: the
// base64 encoding of the largest raw payload plus a generous bounded JSON
// overhead for the path, mode, and expectation fields. The encoded length
// is computed with constant arithmetic: (n+2)/3*4 matches
// base64.StdEncoding.EncodedLen for every n.
const deploymentMaxMetadata = ((deploymentMaxPayload+2)/3)*4 + 4<<20

// deploymentCheckTarget validates a destination before any effect: a clean
// absolute path and a plain permission mode without special bits. The mode
// must keep the file owner-readable so a transfer can never create an
// unreadable or privileged file.
func deploymentCheckTarget(target string, mode os.FileMode) error {
	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return fail("The destination path must be a clean absolute path.")
	}
	if mode > 0o777 || mode&0o7000 != 0 || mode&0o400 == 0 {
		return fail("The destination mode must be a plain permission set.")
	}
	return nil
}

// deploymentSafeDir creates the destination parent as a private directory
// tree. Missing components are created with 0700; existing components are
// left untouched. Symbolic components are resolved afterwards by
// nodeWriteFile so the atomic rename always lands in the real directory
// instead of being redirected mid-write.
func deploymentSafeDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fail("The destination path must be a clean absolute path.")
	}
	check := "/"
	for _, part := range strings.Split(strings.Trim(filepath.Clean(dir), "/"), "/") {
		check = filepath.Join(check, part)
		info, err := os.Lstat(check)
		if os.IsNotExist(err) {
			if err := os.Mkdir(check, 0o700); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fail("The destination parent is not a directory.")
		}
	}
	return nil
}

// nodeWriteFile is the private worker-side transfer receiver. The full
// metadata arrives as exactly one bounded JSON object on standard input, so
// no payload or secret ever appears on argv. The optional "operation" field
// selects the effect; the default is "write" for compatibility:
//
//	write   {"path","mode","data_base64"[,"expected_sha256"|"expected_missing"]}
//	inspect {"path"} -> {"exists":bool,"mode","data_base64","sha256"}
//	remove  {"path","expected_sha256"}
//
// The write is atomic: a temporary file in the same directory receives the
// bytes with the intended mode, is fsynced, and is renamed over the
// destination; any error removes the temporary file and leaves the
// destination untouched. A symbolic link destination or symlinked parent is
// refused, never followed. A compare-before-write expectation is checked
// before any mutation so a later transaction can roll back its own writes.
func nodeWriteFile(in io.Reader) (any, error) {
	data, err := io.ReadAll(io.LimitReader(in, deploymentMaxMetadata+1))
	if err != nil || len(data) > deploymentMaxMetadata {
		return nil, fail("The transfer metadata is missing or too large.")
	}
	meta, err := decode(data)
	if err != nil {
		// decode rejects trailing or duplicate JSON documents.
		return nil, fail("The transfer metadata is not a valid JSON object.")
	}
	switch operation := str(meta["operation"]); operation {
	case "", "write":
		return nodeFileWrite(meta)
	case "inspect":
		return nodeFileInspect(meta)
	case "remove":
		return nodeFileRemove(meta)
	default:
		return nil, fail("The transfer operation is unknown.")
	}
}

// nodeFileExpect validates the optional compare-before-write fields and
// returns them separately: an expected digest for an existing destination,
// or an expected_missing flag for a new one. A present but null or
// wrong-typed field is an error, never a silently disabled expectation.
func nodeFileExpect(meta Object) (string, bool, error) {
	sum, sumGiven := meta["expected_sha256"]
	missing, missingGiven := meta["expected_missing"]
	if sumGiven && sum != nil {
		text, isText := sum.(string)
		if !isText || len(text) != 64 || !isHex(text) {
			return "", false, fail("The expected digest must be a hex sha256 string.")
		}
		if missingGiven && missing != nil {
			return "", false, fail("The expectations must be separate transfers.")
		}
		return strings.ToLower(text), false, nil
	}
	if missingGiven && missing != nil {
		flag, isBool := missing.(bool)
		if !isBool {
			return "", false, fail("The expected_missing field must be boolean.")
		}
		if flag {
			return "", true, nil
		}
	}
	return "", false, nil
}

// nodeFileWrite performs the atomic write with an optional pre-write
// expectation. data_base64 must be a present JSON string; an explicit empty
// string is a valid zero-byte file, while a missing, null, or non-string
// field leaves the target intact and fails before any effect.
func nodeFileWrite(meta Object) (any, error) {
	target := str(meta["path"])
	if target == "" {
		return nil, fail("The destination path must be a clean absolute path.")
	}
	mode, perr := strconv.ParseUint(str(meta["mode"]), 8, 32)
	if perr != nil {
		return nil, fail("The destination mode must be a plain permission set.")
	}
	if err := deploymentCheckTarget(target, os.FileMode(mode)); err != nil {
		return nil, err
	}
	encoded, ok := meta["data_base64"].(string)
	if !ok {
		return nil, fail("The transfer payload must be a base64 string.")
	}
	// The raw payload and its base64 envelope share one bound.
	if len(encoded) > deploymentMaxMetadata {
		return nil, fail("The transfer payload is missing or too large.")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) > deploymentMaxPayload {
		return nil, fail("The transfer payload is missing or too large.")
	}
	expected, expectMissing, err := nodeFileExpect(meta)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(target)
	if err := deploymentSafeDir(dir); err != nil {
		return nil, err
	}
	// Resolve symbolic parents once: the temporary file and the rename both
	// happen inside the real directory, so a symlinked ancestor can never
	// redirect the atomic rename, and the destination itself is checked as
	// a non-symlink regular file.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	final := filepath.Join(resolved, filepath.Base(target))
	info, err := os.Lstat(final)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fail("The destination is a symbolic link.")
		}
		if !info.Mode().IsRegular() {
			return nil, fail("The destination is a directory.")
		}
		// Compare before mutating: a mismatched existing payload is never
		// overwritten, so the transaction can roll back its own writes.
		if expectMissing {
			return nil, fail("The destination already exists.")
		}
		if expected != "" {
			current, err := nodeFileDigest(final)
			if err != nil {
				return nil, err
			}
			if current != expected {
				return nil, fail("The destination content changed.")
			}
		}
	case os.IsNotExist(err):
		if expected != "" {
			return nil, fail("The destination content changed.")
		}
	default:
		return nil, err
	}
	f, err := os.CreateTemp(resolved, ".sparky-*")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(os.FileMode(mode)); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return nil, err
	}
	return Object{"status": "written", "path": final, "sha256": sha256Hex(raw)}, nil
}

// nodeFileInspect reports the exact bytes and mode of a real regular file
// without any directory creation or write. A truly absent path is
// {"exists":false}; symlinks and special files are refused, never followed,
// and permission errors are surfaced instead of being read as absence.
func nodeFileInspect(meta Object) (any, error) {
	target := str(meta["path"])
	if target == "" {
		return nil, fail("The destination path must be a clean absolute path.")
	}
	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return nil, fail("The destination path must be a clean absolute path.")
	}
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return Object{"exists": false}, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fail("The inspected path is not a regular file.")
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return nil, err
	}
	if len(raw) > deploymentMaxPayload {
		return nil, fail("The inspected file is too large.")
	}
	return Object{
		"exists":      true,
		"mode":        fmt.Sprintf("%o", info.Mode().Perm()),
		"data_base64": base64.StdEncoding.EncodeToString(raw),
		"sha256":      sha256Hex(raw),
	}, nil
}

// nodeFileRemove rolls back exactly one own-created file: it refuses to
// remove anything but a real regular file whose current digest matches the
// recorded expectation. An absent path is idempotent success; a changed
// file, symlink, or special file is refused. It is never a general cleanup.
func nodeFileRemove(meta Object) (any, error) {
	target := str(meta["path"])
	if target == "" {
		return nil, fail("The destination path must be a clean absolute path.")
	}
	expected := str(meta["expected_sha256"])
	if len(expected) != 64 || !isHex(expected) {
		return nil, fail("A recorded digest is required for removal.")
	}
	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return nil, fail("The destination path must be a clean absolute path.")
	}
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return Object{"status": "removed", "path": target}, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fail("The removal path is not a regular file.")
	}
	current, err := nodeFileDigest(target)
	if err != nil {
		return nil, err
	}
	if current != strings.ToLower(expected) {
		return nil, fail("The file content changed. Removal refused.")
	}
	if err := os.Remove(target); err != nil {
		return nil, err
	}
	return Object{"status": "removed", "path": target}, nil
}

// nodeFileDigest reads a regular file and hashes it; the path is already
// confirmed non-symlink and regular by the caller.
func nodeFileDigest(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(raw) > deploymentMaxPayload {
		return "", fail("The destination file is too large.")
	}
	return sha256Hex(raw), nil
}

// sha256Hex is the lowercase hex digest of the payload.
func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// isHex reports whether the text is pure lowercase hex digits.
func isHex(text string) bool {
	for _, r := range text {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
