// Bounded transactional deployment journal. Every mutation through the
// journal snapshots the destination with the strict node file protocol,
// registers the undo BEFORE dispatching (the remote may commit and the
// transport may then fail), writes compare-and-swap against the observed
// state, and confirms the final digest and mode. Rollback uses fresh bounded
// contexts independent of the initiating context and attempts every undo in
// reverse order even when earlier ones fail. The journal only ever touches
// its own recorded payloads; an external edit is preserved and reported.
package sparky

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// deploymentJournalBound bounds every journal command, including worker
// uploads and root unit installs. An inherited shorter deadline is honored.
const deploymentJournalBound = 60 * time.Second

// deploymentSSHArgs is the pinned non-interactive transport.
func deploymentSSHArgs(host string) []string {
	return []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", host}
}

// journalHelper is the absolute native Sparky helper path. sudo never
// resolves "~", so root unit operations need the expanded absolute path.
func journalHelper() string { return filepath.Join(home(), ".local/bin/sparky") }

// journalBound caps the context at the journal bound; an inherited earlier
// deadline always wins.
func journalBound(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= deploymentJournalBound {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, deploymentJournalBound)
}

// DeploymentJournal records exactly what it changed on each node so a
// partial deployment can be undone. The configuration is copied at
// construction; later caller mutations cannot redirect effects.
type DeploymentJournal struct {
	cfg   Object
	stage string
	undos []journalUndo
}

// journalUndo is one recorded mutation and its exact previous state.
type journalUndo struct {
	node      string
	path      string
	root      bool
	existed   bool
	prior     []byte
	priorMode os.FileMode
	priorHash string
	ownMode   os.FileMode
	ownHash   string
}

// NewDeploymentJournal deep-copies the cluster configuration and keeps the
// private stage directory for artifact bookkeeping, so a later caller
// mutation of node metadata cannot redirect effects. Node names are
// strictly spark01 or spark02; anything else is refused at every call.
func NewDeploymentJournal(cfg Object, stage string) *DeploymentJournal {
	return &DeploymentJournal{cfg: copyObject(cfg), stage: stage}
}

// journalCheckNode allows only the two real cluster nodes.
func journalCheckNode(node string) error {
	if node != "spark01" && node != "spark02" {
		return fail("The node must be spark01 or spark02.")
	}
	return nil
}

// call dispatches one node file protocol operation. Local normal files run
// the protocol in-process; root unit operations go through
// "sudo -n <absolute sparky helper>" and the worker goes through the pinned
// non-interactive SSH transport, always with the metadata on stdin and
// bounded to at most the journal bound.
func (j *DeploymentJournal) call(ctx context.Context, node string, meta Object, root bool) (Object, error) {
	if err := journalCheckNode(node); err != nil {
		return nil, err
	}
	if node == "spark01" && !root {
		result, err := nodeWriteFile(bytes.NewReader(jsonBytes(meta)))
		if err != nil {
			return nil, err
		}
		out, ok := result.(Object)
		if !ok {
			return nil, fail("The file protocol returned a malformed result.")
		}
		return out, nil
	}
	bounded, cancel := journalBound(ctx)
	defer cancel()
	var args []string
	if node == "spark01" {
		args = []string{"sudo", "-n", journalHelper(), "--json", "node", "write-file"}
	} else {
		host := str(obj(obj(j.cfg["nodes"])["spark02"])["ssh"])
		if err := safeHost(host); err != nil {
			return nil, err
		}
		remote := "~/.local/bin/sparky --json node write-file"
		if root {
			// sudo cannot expand "~" or "$HOME": pass the actual absolute
			// helper path from the configured home layout, safely quoted.
			remote = shellJoin([]string{"sudo", "-n", journalHelper(), "--json", "node", "write-file"})
		}
		args = append(deploymentSSHArgs(host), remote)
	}
	result, err := deploymentRun(bounded, args, jsonBytes(meta))
	if err != nil || result.Code != 0 {
		return nil, fail("The file protocol command did not succeed.")
	}
	out, err := decode([]byte(result.Out))
	if err != nil {
		return nil, fail("The file protocol response is not a valid JSON object.")
	}
	return out, nil
}

// journalSnapshot is the strict parse of one inspect response.
type journalSnapshot struct {
	exists bool
	mode   os.FileMode
	hash   string
	data   []byte
}

// inspect reads the exact current state through the strict protocol; a
// missing digest, mode, payload, or over-large file is refused, never
// guessed.
func (j *DeploymentJournal) inspect(ctx context.Context, node, path string, root bool) (journalSnapshot, error) {
	result, err := j.call(ctx, node, Object{"operation": "inspect", "path": path}, root)
	if err != nil {
		return journalSnapshot{}, err
	}
	exists, ok := result["exists"].(bool)
	if !ok {
		return journalSnapshot{}, fail("The inspect response is malformed.")
	}
	if !exists {
		if len(result) != 1 {
			return journalSnapshot{}, fail("The inspect response is malformed.")
		}
		return journalSnapshot{}, nil
	}
	hash := str(result["sha256"])
	if len(hash) != 64 || !isHex(hash) {
		return journalSnapshot{}, fail("The inspect response is malformed.")
	}
	mode, err := strconv.ParseUint(str(result["mode"]), 8, 32)
	if err != nil || mode > 0o777 {
		return journalSnapshot{}, fail("The inspect response is malformed.")
	}
	// The payload must be a present typed string; a missing, null, or
	// non-string field is an error, never an empty valid file.
	encoded, ok := result["data_base64"].(string)
	if !ok {
		return journalSnapshot{}, fail("The inspect response is malformed.")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) > deploymentMaxPayload {
		return journalSnapshot{}, fail("The inspect response is malformed.")
	}
	if sha256Hex(raw) != strings.ToLower(hash) {
		return journalSnapshot{}, fail("The inspect response is malformed.")
	}
	return journalSnapshot{exists: true, mode: os.FileMode(mode), hash: strings.ToLower(hash), data: raw}, nil
}

// write performs one compare-and-swap mutation with the undo registered
// before dispatch and the final state confirmed afterwards.
func (j *DeploymentJournal) write(ctx context.Context, node, path string, data []byte, mode os.FileMode, root bool) error {
	if err := journalCheckNode(node); err != nil {
		return err
	}
	if err := deploymentCheckTarget(path, mode); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fail("The deployment was cancelled.")
	}
	ownHash := sha256Hex(data)
	before, err := j.inspect(ctx, node, path, root)
	if err != nil {
		return err
	}
	undo := journalUndo{node: node, path: path, root: root, ownHash: ownHash, ownMode: mode}
	if before.exists {
		undo.existed = true
		undo.prior = before.data
		undo.priorMode = before.mode
		undo.priorHash = before.hash
	}
	// Registered before dispatch: the remote may commit and the transport
	// may then fail, and the undo must still exist.
	j.undos = append(j.undos, undo)
	meta := Object{
		"operation":   "write",
		"path":        path,
		"mode":        fmt.Sprintf("%o", mode),
		"data_base64": base64.StdEncoding.EncodeToString(data),
	}
	if before.exists {
		meta["expected_sha256"] = before.hash
	} else {
		meta["expected_missing"] = true
	}
	result, err := j.call(ctx, node, meta, root)
	if err != nil {
		// A transport failure after a remote commit is handled by the
		// registered undo, never by a silent success.
		return err
	}
	if str(result["status"]) == "written" && str(result["sha256"]) == ownHash {
		return nil
	}
	// The response is malformed or uncertain: confirm through a strict inspect.
	after, ierr := j.inspect(ctx, node, path, root)
	if ierr != nil || !after.exists || after.hash != ownHash || after.mode != mode {
		return fail("The write to %s could not be confirmed.", filepath.Base(path))
	}
	return nil
}

// WriteFile installs one payload on one node through the transactional CAS
// file protocol.
func (j *DeploymentJournal) WriteFile(ctx context.Context, node, path string, data []byte, mode os.FileMode) error {
	return j.write(ctx, node, path, data, mode, false)
}

// WriteUnit installs one systemd unit at the fixed root mode through the
// same CAS protocol under sudo, never an untracked raw install.
func (j *DeploymentJournal) WriteUnit(ctx context.Context, node, path, text string) error {
	if !filepath.IsAbs(path) {
		return fail("The unit target path must be absolute.")
	}
	for _, r := range text {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			return fail("Unit content must not contain control characters.")
		}
	}
	return j.write(ctx, node, path, []byte(text), 0o644, true)
}

// Rollback attempts every recorded undo in reverse order with a fresh
// bounded context per undo, independent of the initiating context. All
// undos are attempted even when some fail; the reported error names only
// the artifact base names. A file that was externally edited after the
// write is preserved and reported, never overwritten or removed.
func (j *DeploymentJournal) Rollback() error {
	var failed []string
	for index := len(j.undos) - 1; index >= 0; index-- {
		if err := j.undo(j.undos[index]); err != nil {
			failed = append(failed, filepath.Base(j.undos[index].path))
		}
	}
	if len(failed) > 0 {
		return fail("Rollback needs attention for: %s.", strings.Join(failed, ", "))
	}
	return nil
}

// undo restores or removes exactly one recorded mutation using the journal's
// own dispatch, with a fresh bounded context. Responses and final state are
// verified; a malformed confirmation is an error, never rollback success.
func (j *DeploymentJournal) undo(u journalUndo) error {
	ctx, cancel := context.WithTimeout(context.Background(), deploymentJournalBound)
	defer cancel()
	current, err := j.inspect(ctx, u.node, u.path, u.root)
	if err != nil {
		return err
	}
	if !u.existed {
		if !current.exists {
			return nil
		}
		if current.hash != u.ownHash {
			return fail("The file %s changed externally and was preserved.", filepath.Base(u.path))
		}
		if _, err := j.call(ctx, u.node, Object{
			"operation":       "remove",
			"path":            u.path,
			"expected_sha256": u.ownHash,
		}, u.root); err != nil {
			return err
		}
		after, err := j.inspect(ctx, u.node, u.path, u.root)
		if err != nil || after.exists {
			return fail("The removal of %s could not be confirmed.", filepath.Base(u.path))
		}
		return nil
	}
	// The destination previously existed: an absence now is an external
	// deletion of unknown ownership; it is preserved and reported, never
	// resurrected.
	if !current.exists {
		return fail("The file %s was removed externally; absence preserved.", filepath.Base(u.path))
	}
	switch {
	case current.hash == u.ownHash && current.mode == u.ownMode:
		meta := Object{
			"operation":       "write",
			"path":            u.path,
			"mode":            fmt.Sprintf("%o", u.priorMode),
			"data_base64":     base64.StdEncoding.EncodeToString(u.prior),
			"expected_sha256": u.ownHash,
		}
		if _, err := j.call(ctx, u.node, meta, u.root); err != nil {
			return err
		}
	case current.hash == u.priorHash && current.mode == u.priorMode:
		return nil
	default:
		return fail("The file %s changed externally and was preserved.", filepath.Base(u.path))
	}
	after, err := j.inspect(ctx, u.node, u.path, u.root)
	if err != nil || !after.exists || after.hash != u.priorHash || after.mode != u.priorMode {
		return fail("The restore of %s could not be confirmed.", filepath.Base(u.path))
	}
	return nil
}
