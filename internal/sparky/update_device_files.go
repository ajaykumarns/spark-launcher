package sparky

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Binary inspection is metadata-only. The old file protocol includes base64
// contents and its command output limit is smaller than a release executable.
func (f *updateJournalFiles) inspectMetadata(ctx context.Context, node, path string) (updateFileSnapshot, error) {
	if err := journalCheckNode(node); err != nil {
		return updateFileSnapshot{}, err
	}
	var result updateFileSnapshot
	if node == "spark01" {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return result, nil
		}
		if err != nil {
			return result, err
		}
		if err := updateDeviceOwned(path); err != nil {
			return result, err
		}
		if info.Size() > deploymentMaxPayload {
			return result, fail("The inspected file exceeds the update payload limit.")
		}
		file, err := os.Open(path)
		if err != nil {
			return result, err
		}
		defer file.Close()
		opened, err := file.Stat()
		if err != nil || !os.SameFile(info, opened) {
			return result, fail("The inspected file changed while opening.")
		}
		hash := sha256.New()
		n, err := io.Copy(hash, io.LimitReader(file, deploymentMaxPayload+1))
		if err != nil {
			return result, err
		}
		after, err := file.Stat()
		if err != nil || n != info.Size() || !after.ModTime().Equal(info.ModTime()) || after.Size() != info.Size() {
			return result, fail("The inspected file changed while hashing.")
		}
		result = updateFileSnapshot{exists: true, mode: info.Mode().Perm(), sha: hex.EncodeToString(hash.Sum(nil))}
	} else {
		host := str(obj(obj(f.j.cfg["nodes"])["spark02"])["ssh"])
		if err := safeHost(host); err != nil {
			return result, err
		}
		script := `set -eu
if [ ! -e "$1" ] && [ ! -L "$1" ]; then printf 'missing\n'; exit 0; fi
[ -f "$1" ] && [ ! -L "$1" ]
[ "$(realpath -e -- "$1")" = "$1" ]
stat -c '%a %u %s' -- "$1"
id -u
sha256sum -- "$1"`
		out, err := updateDeviceCommand(ctx, updateDeviceSSH(host, []string{"sh", "-c", script, "sparky-update-inspect", path}), nil)
		if err != nil {
			return result, err
		}
		if out == "missing" {
			return result, nil
		}
		lines := strings.Split(out, "\n")
		if len(lines) != 3 {
			return result, fail("The worker file metadata is unreadable.")
		}
		fields := strings.Fields(lines[0])
		if len(fields) != 3 || fields[1] != strings.TrimSpace(lines[1]) {
			return result, fail("The worker file is not owned by its SSH account.")
		}
		mode, e1 := strconv.ParseUint(fields[0], 8, 32)
		size, e2 := strconv.ParseInt(fields[2], 10, 64)
		digest, ok := updateParseDigest([]byte(lines[2]))
		if e1 != nil || e2 != nil || !ok || size < 0 || size > deploymentMaxPayload {
			return result, fail("The worker file metadata is invalid.")
		}
		result = updateFileSnapshot{exists: true, mode: os.FileMode(mode), sha: digest}
		if filepath.Base(path) == "BUILDINFO.json" && size > updateDeviceBuildInfoBound {
			return result, fail("The build information is too large.")
		}
	}
	if filepath.Base(path) == "BUILDINFO.json" {
		if node == "spark01" {
			info, err := os.Stat(path)
			if err != nil || info.Size() > updateDeviceBuildInfoBound {
				return result, fail("The build information is too large.")
			}
			result.data, err = os.ReadFile(path)
			if err != nil {
				return result, err
			}
		} else {
			snapshot, err := f.j.inspect(ctx, node, path, false)
			if err != nil {
				return result, err
			}
			result.data = snapshot.data
		}
		if sha256Hex(result.data) != result.sha {
			return result, fail("The build information changed during inspection.")
		}
	}
	return result, nil
}

// Keep a known backup before invoking an installer: its receipt can be lost
// after an atomic replacement. Never overwrite an existing recovery file.
func (f *updateJournalFiles) backup(ctx context.Context, node, source, target, expected string) error {
	before, err := f.inspect(ctx, node, source)
	if err != nil {
		return err
	}
	if !before.exists || before.sha != expected {
		return fail("The installed CLI changed before backup.")
	}
	if node == "spark01" {
		if err := deploymentSafeDir(filepath.Dir(target)); err != nil {
			return err
		}
		in, err := os.Open(source)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, io.LimitReader(in, deploymentMaxPayload+1))
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	} else {
		host := str(obj(obj(f.j.cfg["nodes"])["spark02"])["ssh"])
		if err := safeHost(host); err != nil {
			return err
		}
		script := `set -eu
umask 077
mkdir -p -m 700 -- "$(dirname -- "$2")"
[ ! -L "$(dirname -- "$2")" ]
set -C
cat -- "$1" > "$2"`
		if _, err := updateDeviceCommand(ctx, updateDeviceSSH(host, []string{"sh", "-c", script, "sparky-update-backup", source, target}), nil); err != nil {
			return err
		}
	}
	saved, err := f.inspect(ctx, node, target)
	if err != nil {
		return err
	}
	if !saved.exists || saved.sha != expected {
		return fmt.Errorf("The recovery backup on %s did not match the previous CLI", node)
	}
	return nil
}
