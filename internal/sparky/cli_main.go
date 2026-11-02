package sparky

// Stage 3 of the public CLI: Main dispatches every parsed command to the
// saved client, remote, harness and terminal APIs. Public JSON mode prints
// one indented JSON object on stdout and one safe {error, code} envelope on
// stderr; the private node command always answers with one JSON envelope on
// stdout so SSH clients can parse it. Raw diagnostics and credentials never
// reach public output.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

// errTerminatedCause marks a SIGTERM-interrupted operation (exit 143).
var errTerminatedCause = errors.New("terminated")

// cliWorkflowMaxBytes bounds a ComfyUI workflow file at read time (8 MiB).
const cliWorkflowMaxBytes = 8 << 20

// cliInterruptExit and cliTerminateExit are the documented interruption
// codes; remote work on the device may always continue.
const (
	cliInterruptExit  = 130
	cliTerminateExit  = 143
	cliVersionMessage = "sparky " + cliVersion
)

// cliInterruptError is the typed failure for a user-interrupted wait.
type cliInterruptError struct{ terminated bool }

func (e *cliInterruptError) Error() string {
	if e.terminated {
		return "Stopped. Remote work may still continue."
	}
	return "Stopped waiting. Remote work may still continue. Run sparky status to check it."
}

func (e *cliInterruptError) ExitCode() int {
	if e.terminated {
		return cliTerminateExit
	}
	return cliInterruptExit
}

// cliExitCoder is the generic typed-exit-code contract: usage errors (2)
// and interruptions (130/143) implement it, so dispatch code never depends
// on one concrete error type.
type cliExitCoder interface{ ExitCode() int }

// cliExitCode reports the typed exit code of an error through errors.As.
func cliExitCode(err error) int {
	var coded cliExitCoder
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return 1
}

// NodeDispatch is the private node seam: Main calls it synchronously with
// the raw node words so NodeMain owns signals and cleanup. Until node.go
// binds it, invoking `sparky node` reports an explicit error, never a
// success stub.
var NodeDispatch func(args []string, in io.Reader) (any, error)

// uiOpenBrowser is the seam for opening the gateway UI in a browser.
var uiOpenBrowser = cliOpenBrowser

// cliOpenBrowser tries the common desktop openers without exposing argv.
func cliOpenBrowser(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, opener := range []string{"open", "xdg-open"} {
		result, err := runCommand(ctx, []string{opener, url}, nil)
		if err == nil && result.Code == 0 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Signals and output plumbing.

// cliSignalHandler cancels the CLI context on SIGINT/SIGTERM (with a typed
// cause so the harness Cancel seam forwards the user's intent) and stops
// forwarding when the returned func runs.
func cliSignalHandler(cancel context.CancelCauseFunc) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		for s := range ch {
			if s == os.Interrupt {
				cancel(errInterruptCause)
			} else {
				cancel(errTerminatedCause)
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(ch)
	}
}

// cliTerminalOptions builds render options from the actual stream: terminal
// detection and column width via x/term, with a safe COLUMNS fallback.
func cliTerminalOptions(color string, out io.Writer) terminalOptions {
	tty := false
	width := 0
	if f, ok := out.(*os.File); ok {
		fd := int(f.Fd())
		if term.IsTerminal(fd) {
			tty = true
			if w, _, err := term.GetSize(fd); err == nil {
				width = w
			}
		}
	}
	if width <= 0 {
		if c, err := strconv.Atoi(envDefault("COLUMNS", "")); err == nil && c > 0 {
			width = c
		}
	}
	return terminalOptions{Color: color, Width: width, TTY: tty}
}

// cliRender renders one human view with the stream's real options.
func cliRender(inv *cliInvocation, out io.Writer, kind string, data any) string {
	color := ""
	if inv != nil {
		color = inv.Color
	}
	return terminalRender(kind, data, cliTerminalOptions(color, out))
}

// cliEmitView prints the JSON value in JSON mode or the pre-rendered human
// view. JSON bypasses the terminal renderer entirely.
func cliEmitView(inv *cliInvocation, out io.Writer, jsonValue any, human string) {
	if inv.JSON {
		_, _ = out.Write(jsonBytes(jsonValue))
		return
	}
	fmt.Fprintln(out, human)
}

// cliJSONEnvelope writes the single safe error envelope and returns the code.
func cliJSONEnvelope(w io.Writer, message string, code int) int {
	b, _ := json.Marshal(Object{"error": message, "code": code})
	fmt.Fprintln(w, string(b))
	return code
}

// cliErrorExit reports one public error and returns its exit code. Private
// node output goes to stdout as one envelope even on failure; public JSON
// errors go to stderr; human output is a notice on stderr.
func cliErrorExit(inv *cliInvocation, err error, out, errout io.Writer) int {
	code := cliExitCode(err)
	private := inv != nil && inv.Command == "node"
	jsonMode := inv != nil && inv.JSON
	if private || jsonMode {
		stream := errout
		if private {
			stream = out
		}
		return cliJSONEnvelope(stream, err.Error(), code)
	}
	title, tone := "sparky: error", "bright_red"
	var interrupt *cliInterruptError
	if errors.As(err, &interrupt) {
		title, tone = "Interrupted", "yellow"
	}
	fmt.Fprintln(errout, cliRender(inv, errout, "notice", Object{
		"title": title, "message": err.Error(), "tone": tone}))
	return code
}

// cliInterruptErr converts a canceled-context failure into the typed
// interruption failure so remote work is reported as "may continue".
func cliInterruptErr(ctx context.Context, err error) error {
	if err == nil || !errors.Is(err, context.Canceled) {
		return err
	}
	switch {
	case errors.Is(context.Cause(ctx), errInterruptCause):
		return &cliInterruptError{terminated: false}
	case errors.Is(context.Cause(ctx), errTerminatedCause):
		return &cliInterruptError{terminated: true}
	}
	return err
}

// ---------------------------------------------------------------------------
// Main.

// Main runs one public CLI invocation and returns the process exit code.
// Exit codes: 0 success, 1 runtime failure, 2 usage, 130 interrupt,
// 143 terminate; a launched harness child's exact status (including 143)
// is returned unchanged.
func Main(args []string, in io.Reader, out, errout io.Writer) int {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	// The seam globals are process-wide; Main restores them so tests can
	// call it repeatedly in one process without leakage or races.
	origSignalContext, origProgressOut := harnessSignalContext, cliProgressOut
	defer func() { harnessSignalContext, cliProgressOut = origSignalContext, origProgressOut }()
	harnessSignalContext = func() context.Context { return ctx }
	// Progress messages always go to stderr like the Python launcher.
	cliProgressOut = errout

	inv, err := cliParse(args)
	if err != nil {
		return cliErrorExit(inv, err, out, errout)
	}

	// Help and version need no configuration or network and are plain text
	// even in JSON mode; --json never turns them into a model query.
	if inv.Help || inv.Version {
		if inv.Version && !inv.Help {
			fmt.Fprintln(out, cliVersionMessage)
		} else {
			fmt.Fprintln(out, cliHelpText)
		}
		return 0
	}

	// The private node protocol always answers with one JSON envelope on
	// stdout, success or failure, and is dispatched synchronously so
	// NodeMain owns signals and finishes its own cleanup. The public signal
	// handler is deliberately NOT installed here: the private controller and
	// supervisor own their own signal completion.
	if inv.Command == "node" {
		return cliNodeCommand(inv, in, out)
	}

	// Public commands install the interrupt handler for their own lifetime.
	stopSignals := cliSignalHandler(cancel)
	defer stopSignals()

	// These commands work without an existing configuration file.
	switch inv.Command {
	case "init":
		return cliInitCommand(inv, out, errout)
	case "logs":
		return cliLogsCommand(inv, out)
	}

	cfg, err := clientLoadConfig()
	if err != nil {
		return cliErrorExit(inv, err, out, errout)
	}
	return cliDispatch(ctx, cfg, inv, in, out, errout)
}

// cliNodeCommand runs the private protocol through the dispatch seam.
func cliNodeCommand(inv *cliInvocation, in io.Reader, out io.Writer) int {
	if NodeDispatch == nil {
		return cliJSONEnvelope(out, "The private node dispatcher is not available in this build.", 1)
	}
	value, err := NodeDispatch(inv.NodeArgs, in)
	if err != nil {
		return cliJSONEnvelope(out, err.Error(), cliExitCode(err))
	}
	if value != nil {
		_, _ = out.Write(jsonBytes(value))
	}
	return 0
}

// cliDispatch routes one configured public command. Every step reports a
// canceled context as "remote work may continue" (130/143).
func cliDispatch(ctx context.Context, cfg clientConfig, inv *cliInvocation, in io.Reader, out, errout io.Writer) int {
	switch inv.Command {
	case "status":
		data, err := cliRemoteStatus(ctx, cfg)
		if err != nil {
			return cliErrorExit(inv, cliInterruptErr(ctx, err), out, errout)
		}
		cliEmitView(inv, out, data, cliRender(inv, out, "status", data))
		return 0
	case "devices":
		data, err := cliDevices(ctx, cfg)
		if err != nil {
			return cliErrorExit(inv, cliInterruptErr(ctx, err), out, errout)
		}
		cliEmitView(inv, out, data, cliRender(inv, out, "devices", data))
		if boolean(data["ok"]) {
			return 0
		}
		return 1
	case "models":
		return cliModelsCommand(ctx, cfg, inv, out, errout)
	case "ocr":
		return cliOCRCommand(ctx, cfg, inv, out, errout)
	case "plan":
		data, err := cliRemoteRPC(ctx, cfg, "plan", Object{
			"action": inv.Positional[0], "model": inv.Positional[1], "on": cliValueOrNil(inv, "on")})
		if err != nil {
			return cliErrorExit(inv, cliInterruptErr(ctx, err), out, errout)
		}
		cliEmitView(inv, out, data, cliRender(inv, out, "plan", data))
		return 0
	case "image-profile":
		data, err := cliRemoteRPC(ctx, cfg, "image-profile", Object{
			"mode": cliPositionalOr(inv, "mode", "show"), "dry_run": inv.Flags["dry-run"]})
		if err != nil {
			return cliErrorExit(inv, cliInterruptErr(ctx, err), out, errout)
		}
		return cliEmitResult(inv, "image-profile", data, out)
	case "config":
		return cliConfigCommand(ctx, cfg, inv, out, errout)
	case "run":
		return cliRunCommand(ctx, cfg, inv, out, errout)
	case "load", "unload":
		return cliLoadCommand(ctx, cfg, inv, out, errout)
	case "sync":
		return cliSyncCommand(ctx, cfg, inv, out, errout)
	case "doctor":
		data := cliDoctor(cfg, cliPositionalOr(inv, "harness", ""))
		cliEmitView(inv, out, data, cliRender(inv, out, "doctor", data))
		if boolean(data["ok"]) {
			return 0
		}
		return 1
	case "ui":
		return cliUICommand(cfg, inv, out, errout)
	case "admin":
		return cliAdminCommand(ctx, cfg, inv, in, out, errout)
	}
	if contains(cliHarnessCommands, inv.Command) {
		return cliHarnessCommand(ctx, cfg, inv, in, out, errout)
	}
	return cliErrorExit(inv, usagef("Choose a command%s", cliUsageSuffix), out, errout)
}

// cliValueOrNil returns an option value or JSON null when absent.
func cliValueOrNil(inv *cliInvocation, name string) any {
	if v, ok := inv.Values[name]; ok {
		return v
	}
	return nil
}

// cliPositionalOr returns a positional or an option value ("" is absent)
// with a fallback default.
func cliPositionalOr(inv *cliInvocation, name, d string) string {
	if len(inv.Positional) > 0 && len(posNamesFor(inv.Command)) > 0 &&
		posNamesFor(inv.Command)[0] == name {
		return inv.Positional[0]
	}
	if v, ok := inv.Values[name]; ok && v != "" {
		return v
	}
	return d
}

// ---------------------------------------------------------------------------
// Command families.

// cliModelsCommand implements models [--offline|--installed] [MODEL].
func cliModelsCommand(ctx context.Context, cfg clientConfig, inv *cliInvocation, out, errout io.Writer) int {
	if inv.Flags["installed"] {
		data, err := cliRemoteStatus(ctx, cfg)
		if err != nil {
			return cliErrorExit(inv, cliInterruptErr(ctx, err), out, errout)
		}
		cliEmitView(inv, out, data, cliRender(inv, out, "inventory", data))
		return 0
	}
	offline := inv.Flags["offline"]
	catalog, err := clientFetchCatalog(cfg, offline)
	if err != nil {
		return cliErrorExit(inv, cliInterruptErr(ctx, err), out, errout)
	}
	if offline {
		catalog["offline"] = true
	}
	if len(inv.Positional) > 0 {
		model, err := clientResolve(catalog, inv.Positional[0])
		if err != nil {
			return cliErrorExit(inv, err, out, errout)
		}
		cliEmitView(inv, out, model, cliRenderModel(inv, out, catalog, model))
		return 0
	}
	cliEmitView(inv, out, catalog, cliRender(inv, out, "models", catalog))
	return 0
}

// cliRenderModel renders one model with its catalog context.
func cliRenderModel(inv *cliInvocation, out io.Writer, catalog, model Object) string {
	opts := cliTerminalOptions(inv.Color, out)
	return formatModelsView(catalog, model, opts.Width, useColor(opts))
}

// cliOCRCommand sends one node ocr request.
func cliOCRCommand(ctx context.Context, cfg clientConfig, inv *cliInvocation, out, errout io.Writer) int {
	if inv.Positional[0] == "install" && !inv.JSON {
		fmt.Fprintln(errout, "Installing OCR on Spark01. This can take several minutes.")
	}
	data, err := cliRemoteOCR(ctx, cfg, inv.Positional[0], inv.Values["repo"], inv.Values["ref"], inv.Flags["rebuild"])
	if err != nil {
		return cliErrorExit(inv, cliInterruptErr(ctx, err), out, errout)
	}
	cliEmitView(inv, out, data, cliRender(inv, out, "ocr", data))
	return 0
}

// cliConfigCommand implements config show|validate|placement over the
// remote RPC surface.
func cliConfigCommand(ctx context.Context, cfg clientConfig, inv *cliInvocation, out, errout io.Writer) int {
	var operation string
	var payload Object
	switch inv.Positional[0] {
	case "show":
		operation, payload = "config", Object{"model": cliPositionalOrNil(inv, 1)}
	case "validate":
		operation, payload = "validate", Object{"model": nil}
	case "placement":
		operation = "placement"
		payload = Object{"model": inv.Positional[1], "on": inv.Positional[2]}
	}
	data, err := cliRemoteRPC(ctx, cfg, operation, payload)
	if err != nil {
		return cliErrorExit(inv, cliInterruptErr(ctx, err), out, errout)
	}
	if operation == "config" {
		cliEmitView(inv, out, data, cliRender(inv, out, "config", data))
		return 0
	}
	return cliEmitResult(inv, operation, data, out)
}

// cliPositionalOrNil returns a positional or JSON null.
func cliPositionalOrNil(inv *cliInvocation, index int) any {
	if len(inv.Positional) > index {
		return inv.Positional[index]
	}
	return nil
}

// cliEmitResult merges the action label for the human result view and
// prints the raw data in JSON mode.
func cliEmitResult(inv *cliInvocation, action string, data Object, out io.Writer) int {
	view := copyObject(data)
	view["action"] = action
	cliEmitView(inv, out, data, cliRender(inv, out, "result", view))
	return 0
}

// cliRunCommand reads a bounded ComfyUI API workflow and submits it.
func cliRunCommand(ctx context.Context, cfg clientConfig, inv *cliInvocation, out, errout io.Writer) int {
	workflow, err := cliReadWorkflow(inv.Positional[1])
	if err != nil {
		return cliErrorExit(inv, err, out, errout)
	}
	stop := cliProgress("Submitting ComfyUI workflow...", !inv.JSON, false)
	data, err := cliRemoteRPC(ctx, cfg, "run", Object{"workflow": workflow, "on": cliValueOrNil(inv, "on")})
	stop()
	if err != nil {
		return cliErrorExit(inv, cliInterruptErr(ctx, err), out, errout)
	}
	if str(data["prompt_id"]) == "" {
		return cliErrorExit(inv, fail("ComfyUI did not accept the workflow. Check its node types and inputs."), out, errout)
	}
	return cliEmitResult(inv, "run", data, out)
}

// cliReadWorkflow validates one exported ComfyUI API workflow file: a
// bounded single JSON object, either the API graph directly or a "prompt"
// wrapper, never the UI nodes/links format. The bound is enforced at read
// time so an oversized file is never fully allocated.
func cliReadWorkflow(path string) (Object, error) {
	p := expand(path)
	info, statErr := os.Stat(p)
	if statErr != nil || !info.Mode().IsRegular() {
		return nil, fail("Workflow file not found: %s. Export a ComfyUI API workflow first.", p)
	}
	file, err := os.Open(p)
	if err != nil {
		return nil, fail("Workflow file not found: %s. Export a ComfyUI API workflow first.", p)
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, cliWorkflowMaxBytes+1))
	if cerr := file.Close(); readErr != nil || cerr != nil {
		return nil, fail("Could not read the workflow file %s.", p)
	}
	if len(raw) > cliWorkflowMaxBytes {
		return nil, fail("The workflow file is too large.")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var doc Object
	if err := decoder.Decode(&doc); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			before := raw[:syntax.Offset]
			line := 1 + bytes.Count(before, []byte("\n"))
			column := int64(syntax.Offset)
			if last := bytes.LastIndexByte(before, '\n'); last >= 0 {
				column = int64(syntax.Offset) - int64(last) - 1
			}
			return nil, fail("Invalid workflow JSON in %s at line %d, column %d.", p, line, column)
		}
		return nil, fail("Invalid workflow JSON in %s at line 1, column 1.", p)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fail("Invalid workflow JSON in %s at line 1, column 1.", p)
	}
	graph := doc
	if prompt, ok := doc["prompt"].(map[string]any); ok {
		graph = prompt
	}
	reject := fail("Use a ComfyUI API workflow, not the UI workflow format. Export it in API format from ComfyUI.")
	if graph == nil || len(graph) == 0 {
		return nil, reject
	}
	if _, has := graph["nodes"]; has {
		return nil, reject
	}
	if _, has := graph["links"]; has {
		return nil, reject
	}
	for _, nodeAny := range graph {
		node, ok := nodeAny.(map[string]any)
		if !ok {
			return nil, reject
		}
		if str(node["class_type"]) == "" {
			return nil, fail("Each API workflow node needs class_type and inputs. Export the workflow in API format from ComfyUI.")
		}
		if _, ok := node["inputs"].(map[string]any); !ok {
			return nil, fail("Each API workflow node needs class_type and inputs. Export the workflow in API format from ComfyUI.")
		}
	}
	return doc, nil
}

// cliLoadCommand implements load/unload with human plan notes and progress.
// A forced or JSON operation goes straight to the remote command.
func cliLoadCommand(ctx context.Context, cfg clientConfig, inv *cliInvocation, out, errout io.Writer) int {
	action, model := inv.Command, inv.Positional[0]
	force := inv.Flags["force"]
	if !inv.JSON && !force {
		plan, err := cliRemoteRPC(ctx, cfg, "plan", Object{"action": action, "model": model})
		if err != nil {
			return cliErrorExit(inv, cliInterruptErr(ctx, err), out, errout)
		}
		conflicts := []string{}
		for _, stepAny := range arr(plan["steps"]) {
			step := obj(stepAny)
			if str(step["action"]) == "unload" && str(step["workload"]) != str(plan["workload"]) {
				conflicts = append(conflicts, str(step["workload"]))
			}
		}
		if len(conflicts) > 0 {
			fmt.Fprintf(errout, "Will unload after active requests finish: %s.\n", strings.Join(conflicts, ", "))
		}
		if str(plan["recipe_validation"]) == "prepared_not_live_tested" {
			fmt.Fprintln(errout, "This placement recipe has not passed a live model load test.")
		}
	}
	message := "Waiting to unload " + model + " after active requests finish."
	if action == "load" {
		message = "Loading " + model + ". This can take several minutes."
	}
	if force {
		message = "Forcing " + action + " " + model + ". Conflicting language requests will stop."
	}
	stop := cliProgress(message, !inv.JSON, force)
	data, err := cliRemoteResult(ctx, cfg, action, model, force)
	stop()
	if err != nil {
		return cliErrorExit(inv, cliInterruptErr(ctx, err), out, errout)
	}
	return cliEmitResult(inv, action, data, out)
}

// cliSyncCommand refreshes the Hermes provider and reports the exact
// {status, backup, models} shape, using the explicit changed result.
func cliSyncCommand(ctx context.Context, cfg clientConfig, inv *cliInvocation, out, errout io.Writer) int {
	catalog, err := clientFetchCatalog(cfg, false)
	if err != nil {
		return cliErrorExit(inv, cliInterruptErr(ctx, err), out, errout)
	}
	backup, changed, err := harnessSyncHermesChanged(cfg, catalog, token())
	if err != nil {
		return cliErrorExit(inv, err, out, errout)
	}
	status := "current"
	if changed {
		status = "updated"
	}
	data := Object{"status": status, "backup": nil, "models": len(arr(catalog["models"]))}
	if backup != "" {
		data["backup"] = backup
	}
	message := "Hermes Spark provider is current. Unrelated providers were kept."
	if backup != "" {
		message += "\nPrivate backup: " + backup
	}
	cliEmitView(inv, out, data, cliRender(inv, out, "notice", Object{
		"title": "Hermes settings", "message": message, "tone": "bright_green"}))
	return 0
}

// cliDoctor checks gateway access and harness installation; never inference.
func cliDoctor(cfg clientConfig, harness string) Object {
	checks := []any{}
	catalog, err := clientFetchCatalog(cfg, false)
	if err != nil {
		checks = append(checks, Object{"name": "gateway", "ok": false, "message": err.Error()})
	} else {
		checks = append(checks, Object{"name": "gateway", "ok": true,
			"message": fmt.Sprintf("Reachable; %d model routes.", len(arr(catalog["models"])))})
	}
	names := harnessNames
	if harness != "" {
		names = []string{harness}
	}
	for _, name := range names {
		binary, err := harnessExecutable(cfg, name)
		if err != nil {
			checks = append(checks, Object{"name": name, "ok": false, "message": err.Error()})
			continue
		}
		checks = append(checks, Object{"name": name, "ok": true, "message": binary})
	}
	ok := true
	for _, checkAny := range checks {
		if !boolean(checkAny.(Object)["ok"]) {
			ok = false
		}
	}
	return Object{"ok": ok, "checks": checks, "inference": false}
}

// cliUICommand prints the URL in JSON mode or opens it in a browser.
func cliUICommand(cfg clientConfig, inv *cliInvocation, out, errout io.Writer) int {
	url := str(cfg.Extra["admin_url"])
	if url == "" {
		return cliErrorExit(inv, fail("Set admin_url in %s to your gateway UI URL.", clientPath()), out, errout)
	}
	if inv.JSON {
		_, _ = out.Write(jsonBytes(Object{"url": url}))
		return 0
	}
	message := "Open this gateway UI URL in your browser: " + url
	if uiOpenBrowser(url) {
		message = "Opened gateway UI: " + url
	}
	fmt.Fprintln(out, cliRender(inv, out, "notice", Object{
		"title": "Gateway UI", "message": message, "tone": "bright_green"}))
	return 0
}

// cliAdminCommand opens an interactive SSH session (never JSON) or
// configures the gateway protocol routes remotely.
func cliAdminCommand(ctx context.Context, cfg clientConfig, inv *cliInvocation, in io.Reader, out, errout io.Writer) int {
	switch inv.Positional[0] {
	case "ssh":
		if inv.JSON {
			return cliErrorExit(inv, &usageError{msg: "Interactive SSH does not support --json. Run sparky admin ssh spark01."}, out, errout)
		}
		target := "spark01"
		if len(inv.Positional) > 1 {
			target = inv.Positional[1]
		}
		code, err := cliAdmin(ctx, cfg, target, in, out, errout)
		if err != nil {
			return cliErrorExit(inv, err, out, errout)
		}
		return code
	case "enable-harnesses":
		stop := cliProgress("Configuring gateway protocol routes...", !inv.JSON, false)
		data, err := cliRemoteResult(ctx, cfg, "gateway", "", false)
		stop()
		if err != nil {
			return cliErrorExit(inv, cliInterruptErr(ctx, err), out, errout)
		}
		return cliEmitResult(inv, "gateway", data, out)
	}
	return cliErrorExit(inv, usagef("Choose a command%s", cliUsageSuffix), out, errout)
}

// cliHarnessCommand implements the four harness launchers: a valid project
// directory is required before any effect, dry-run/prepare print JSON, and
// a real launch returns the child's exact exit status.
func cliHarnessCommand(ctx context.Context, cfg clientConfig, inv *cliInvocation, in io.Reader, out, errout io.Writer) int {
	extra := inv.Native
	project := clientResolveProject(inv.Values["project"])
	if info, err := os.Stat(project); err != nil || !info.IsDir() {
		return cliErrorExit(inv, fail("Project directory does not exist: %s. Choose it with --project.", project), out, errout)
	}
	offline := inv.Flags["offline"]
	catalog, err := clientFetchCatalog(cfg, offline)
	if err != nil {
		return cliErrorExit(inv, cliInterruptErr(ctx, err), out, errout)
	}
	if offline {
		catalog["offline"] = true
	}
	name := inv.Values["model"]
	if name == "" {
		name = cfg.DefaultModel
	}
	if name == "" {
		name = cfg.DefaultModel
	}
	model, err := clientResolve(catalog, name)
	if err != nil {
		return cliErrorExit(inv, err, out, errout)
	}
	plan, err := buildHarnessPlan(cfg, catalog, model, inv.Command, project, token())
	if err != nil {
		return cliErrorExit(inv, err, out, errout)
	}
	prepare := inv.Flags["prepare"]
	if inv.Flags["dry-run"] || prepare {
		if prepare {
			if err := harnessPrepare(cfg, catalog, plan); err != nil {
				return cliErrorExit(inv, err, out, errout)
			}
		}
		plan["prepared"] = prepare
		plan["gateway"] = cfg.Gateway
		plan["offline"] = offline
		plan["extra_arguments"] = anyStrings(extra)
		args := arr(plan["args"])
		for _, a := range extra {
			args = append(args, a)
		}
		plan["args"] = args
		cliEmitView(inv, out, plan, cliRender(inv, out, "launch", plan))
		return 0
	}
	if offline {
		fmt.Fprintln(errout, "Using cached model metadata. The server was not checked.")
	}
	code, err := harnessLaunch(cfg, catalog, plan, extra, in, out, errout)
	if err != nil {
		return cliErrorExit(inv, err, out, errout)
	}
	return code
}

// cliInitCommand creates the client configuration file exclusively (no
// overwrite, including a broken symlink), 0600 private, in a 0700 site dir.
func cliInitCommand(inv *cliInvocation, out, errout io.Writer) int {
	path := clientPath()
	if _, err := os.Lstat(path); err == nil {
		return cliErrorExit(inv, fail("Client configuration already exists at %s. Edit that file instead.", path), out, errout)
	} else if !os.IsNotExist(err) {
		return cliErrorExit(inv, err, out, errout)
	}
	cfg, err := clientValidateConfig(Object{
		"version":         1,
		"gateway":         inv.Values["gateway"],
		"model_prefix":    inv.Values["prefix"],
		"default_model":   inv.Values["prefix"] + "spark-auto",
		"timeout_seconds": 7200,
		"compact_percent": 95,
		"admin_ssh_host":  inv.Values["admin-host"],
		"worker_ssh_host": inv.Values["worker-host"],
		"remote_root":     inv.Values["remote-root"],
		"admin_url":       inv.Values["admin-url"],
	})
	if err != nil {
		return cliErrorExit(inv, err, out, errout)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return cliErrorExit(inv, err, out, errout)
	}
	encoded, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return cliErrorExit(inv, err, out, errout)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			return cliErrorExit(inv, fail("Client configuration already exists at %s. Edit that file instead.", path), out, errout)
		}
		return cliErrorExit(inv, err, out, errout)
	}
	_, werr := file.Write(append(encoded, '\n'))
	cerr := file.Close()
	if err := firstError(werr, cerr); err != nil {
		return cliErrorExit(inv, err, out, errout)
	}
	cliEmitView(inv, out, Object{"status": "created", "path": path}, cliRender(inv, out, "notice", Object{
		"title":   "Client configured",
		"message": "Client configuration created: " + path + "\nRun sparky models to check gateway access.",
		"tone":    "bright_green"}))
	return 0
}

// cliLogsCommand prints local harness launches, skipping corrupt records.
func cliLogsCommand(inv *cliInvocation, out io.Writer) int {
	data := cliReadLogs(inv.Limit)
	cliEmitView(inv, out, data, cliRender(inv, out, "logs", data))
	return 0
}

// cliReadLogs collects launch records newest first with skipped file names.
func cliReadLogs(limit int) Object {
	records := []any{}
	skipped := []string{}
	dir := filepath.Join(stateDir(), "launches")
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			raw, rerr := os.ReadFile(filepath.Join(dir, entry.Name()))
			var record Object
			if rerr == nil {
				record, rerr = decode(raw)
			}
			if rerr != nil || str(record["started_at"]) == "" {
				skipped = append(skipped, entry.Name())
				continue
			}
			records = append(records, record)
		}
	}
	sort.SliceStable(records, func(i, j int) bool {
		return str(obj(records[i])["started_at"]) > str(obj(records[j])["started_at"])
	})
	if len(records) > limit {
		records = records[:limit]
	}
	return Object{"records": records, "skipped_files": skipped}
}
