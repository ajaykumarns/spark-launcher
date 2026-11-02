package sparky

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Object = map[string]any

func obj(v any) Object {
	if x, ok := v.(map[string]any); ok && x != nil {
		return x
	}
	return Object{}
}
func arr(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []string:
		// Typed string collections produced by Go code are treated like
		// JSON arrays instead of silently disappearing.
		out := make([]any, 0, len(x))
		for _, s := range x {
			out = append(out, s)
		}
		return out
	}
	return nil
}
func str(v any) string { s, _ := v.(string); return s }
func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int8:
		return float64(x)
	case int16:
		return float64(x)
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case uint:
		return float64(x)
	case uint8:
		return float64(x)
	case uint16:
		return float64(x)
	case uint32:
		return float64(x)
	case uint64:
		return float64(x)
	case json.Number:
		n, err := x.Float64()
		if err != nil {
			// Malformed numbers must never become a valid zero.
			return math.NaN()
		}
		return n
	}
	return 0
}
func integer(v any) bool {
	switch x := v.(type) {
	case float64:
		return !math.IsNaN(x) && !math.IsInf(x, 0) && x == math.Trunc(x) && x >= math.MinInt64 && x <= math.MaxInt64
	case float32:
		return integer(float64(x))
	case int, int8, int16, int32, int64:
		return true
	case uint, uint8, uint16, uint32, uint64:
		return true
	case json.Number:
		if _, err := strconv.ParseInt(x.String(), 10, 64); err == nil {
			return true
		}
		_, err := strconv.ParseUint(x.String(), 10, 64)
		return err == nil
	}
	return false
}
func boolean(v any) bool { b, _ := v.(bool); return b }
func value(o Object, k string, d any) any {
	if v, ok := o[k]; ok {
		return v
	}
	return d
}
func stringsOf(v any) []string {
	out := []string{}
	switch xs := v.(type) {
	case []string:
		return xs
	case []any:
		for _, x := range xs {
			out = append(out, str(x))
		}
	}
	return out
}
func anyStrings(xs []string) []any {
	out := []any{}
	for _, x := range xs {
		out = append(out, x)
	}
	return out
}
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
func keys(o Object) []string {
	out := make([]string, 0, len(o))
	for k := range o {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// copyValue deep-copies configuration data without any JSON roundtrip, so
// exact numeric spelling (json.Number, uint64 seeds), booleans and nulls
// survive the copy unchanged.
func copyValue(v any) any {
	switch value := v.(type) {
	case Object:
		if value == nil {
			return nil
		}
		out := make(Object, len(value))
		for k, item := range value {
			out[k] = copyValue(item)
		}
		return out
	case []any:
		if value == nil {
			return nil
		}
		out := make([]any, 0, len(value))
		for _, item := range value {
			out = append(out, copyValue(item))
		}
		return out
	case []string:
		if value == nil {
			return nil
		}
		out := make([]string, len(value))
		copy(out, value)
		return out
	case []Object:
		// Typed collections used by module code must be copied, never aliased.
		if value == nil {
			return nil
		}
		out := make([]Object, 0, len(value))
		for _, item := range value {
			out = append(out, copyObject(item))
		}
		return out
	case map[any]any:
		if value == nil {
			return nil
		}
		// YAML metadata may carry non-string keys. Converting them into a
		// string-keyed map would silently truncate data, so the typed map
		// is deep-copied with its original keys preserved.
		out := make(map[any]any, len(value))
		for k, item := range value {
			out[k] = copyValue(item)
		}
		return out
	}
	return v
}
func copyObject(o Object) Object {
	out := make(Object, len(o))
	for k, v := range o {
		out[k] = copyValue(v)
	}
	return out
}
func merge(a, b Object) Object {
	out := copyObject(a)
	for k, v := range b {
		out[k] = v
	}
	return out
}
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func token() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func home() string { h, _ := os.UserHomeDir(); return h }
func expand(p string) string {
	if p == "~" {
		return home()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home(), p[2:])
	}
	return p
}
func envDefault(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func stateDir() string {
	return expand(envDefault("SPARKY_STATE_DIR", filepath.Join(home(), ".local/state/sparky")))
}
func clientPath() string {
	return expand(envDefault("SPARKY_CONFIG", filepath.Join(home(), ".config/sparky/client.json")))
}
func fail(s string, args ...any) error { return fmt.Errorf(s, args...) }

// decode parses exactly one JSON object, preserving exact numeric spelling
// through json.Number and rejecting trailing data.
func decode(b []byte) (Object, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var o Object
	if err := dec.Decode(&o); err != nil || o == nil {
		return nil, fail("Expected a JSON object.")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fail("Expected a JSON object.")
	}
	return o, nil
}
func readObject(path string) (Object, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decode(b)
}
func jsonBytes(v any) []byte { b, _ := json.MarshalIndent(v, "", "  "); return append(b, '\n') }
func atomicWrite(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".sparky-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
func writeObject(path string, o any) error {
	// Encoding must succeed before any filesystem effect; an unsupported
	// value (NaN, channel, cyclic data) must never truncate a valid file.
	b, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return fail("Cannot encode state as JSON: %v.", err)
	}
	return atomicWrite(path, append(b, '\n'), 0600)
}
func withFileLock(path string, wait bool, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	flags := syscall.LOCK_EX
	if !wait {
		flags |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), flags); err != nil {
		return fail("Another Sparky operation holds this lock.")
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

type CommandResult struct {
	Out, Err string
	Code     int
}
type Runner func(context.Context, []string, []byte) (CommandResult, error)

// Bounded subprocess capture. Limits are deliberately generous for node
// JSON and probe output while still preventing unbounded memory growth from
// a runaway descendant; the values are seams so tests can shrink them.
var (
	runCommandStdoutLimit = 8 << 20 // 8 MiB of JSON/probe output
	runCommandStderrLimit = 1 << 20 // 1 MiB of diagnostics
	// runCommandWaitDelay bounds how long cmd.Wait waits for I/O pipes
	// after the process exits or the context is canceled — a descendant
	// inheriting an open pipe can no longer hang a cleanup deadline.
	runCommandWaitDelay = 5 * time.Second
	errOutputLimit      = errors.New("command output exceeded the capture limit")
)

// boundedWriter captures at most limit bytes; on overflow it trips the
// provided cancel (which must kill this command only) and stops copying.
type boundedWriter struct {
	buf     bytes.Buffer
	limit   int
	tripped bool
	cancel  func()
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	room := w.limit - w.buf.Len()
	if room <= 0 {
		if !w.tripped {
			w.tripped = true
			w.cancel()
		}
		return 0, errOutputLimit
	}
	n := len(p)
	if n > room {
		n = room
	}
	w.buf.Write(p[:n])
	if n < len(p) {
		if !w.tripped {
			w.tripped = true
			w.cancel()
		}
		return n, errOutputLimit
	}
	return n, nil
}

// runCommand runs a subprocess with exact stdin bytes and bounded output
// capture. It kills only its own process on overflow or context
// cancellation, bounds post-cancel pipe waits via WaitDelay, and never
// reports success (Code 0, nil error) after a cancellation or overflow.
// Errors are safe: no argv, payload, or output contents are included.
func runCommand(ctx context.Context, args []string, input []byte) (CommandResult, error) {
	if len(args) == 0 {
		return CommandResult{}, fail("Missing command.")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(runCtx, args[0], args[1:]...)
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = runCommandWaitDelay
	cmd.Stdin = bytes.NewReader(input)
	stdout := &boundedWriter{limit: runCommandStdoutLimit, cancel: cancel}
	stderr := &boundedWriter{limit: runCommandStderrLimit, cancel: cancel}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return CommandResult{}, fail("Command could not be started.")
	}
	err := cmd.Wait()
	// cmd.Wait has reaped the process; with WaitDelay set it has also
	// closed any pipes and joined the internal copy goroutines, so
	// returning here cannot leak goroutines or zombie children.
	result := CommandResult{Out: stdout.buf.String(), Err: stderr.buf.String()}
	if stdout.tripped || stderr.tripped {
		return result, fail("Command output exceeded the capture limit.")
	}
	if err != nil {
		// The command was killed by the caller's context cancellation
		// (runCtx is done) or by its own overflow cancel; never present a
		// killed run as success.
		if runCtx.Err() != nil {
			return result, fail("Command was canceled before completion.")
		}
		if code, ok := exitCode(err); ok && code >= 0 {
			result.Code = code
			return result, nil
		}
		if errors.Is(err, exec.ErrWaitDelay) {
			return result, fail("Command descendants kept the output pipes open; capture was bounded.")
		}
		if code, ok := exitCode(err); ok && code < 0 {
			return result, fail("Command terminated abnormally.")
		}
		return result, fail("Command could not be started.")
	}
	if runCtx.Err() != nil {
		return result, fail("Command was canceled before completion.")
	}
	return result, nil
}

func exitCode(err error) (int, bool) {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), true
	}
	return 0, false
}
func command(args []string, input []byte, timeout time.Duration) (CommandResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return runCommand(ctx, args, input)
}
func checked(args []string, input []byte, timeout time.Duration) (string, error) {
	r, err := command(args, input, timeout)
	if err != nil || r.Code != 0 {
		return "", fail("Device operation failed: %s. Check the local service log.", filepath.Base(args[0]))
	}
	return r.Out, nil
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func shellJoin(args []string) string {
	out := []string{}
	for _, a := range args {
		out = append(out, shellQuote(a))
	}
	return strings.Join(out, " ")
}
func readEnv(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), "\"'")
		}
	}
	return out, nil
}

func httpClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func request(ctx context.Context, method, url string, payload any, headers map[string]string, timeout time.Duration) ([]byte, int, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, 0, fail("Invalid server address.")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient(timeout).Do(req)
	if err != nil {
		return nil, 0, fail("Cannot reach the server. Check Tailscale and the gateway.")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024+1))
	if err != nil || len(b) > 8*1024*1024 {
		return nil, resp.StatusCode, fail("Cannot read the server response.")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return b, resp.StatusCode, fail("Server returned HTTP %d.", resp.StatusCode)
	}
	return b, resp.StatusCode, nil
}
func requestObject(ctx context.Context, method, url string, payload any, headers map[string]string, timeout time.Duration, safeError bool) (Object, error) {
	b, _, err := request(ctx, method, url, payload, headers, timeout)
	if err != nil {
		if safeError {
			if o, e := decode(b); e == nil && str(o["error"]) != "" {
				return nil, errors.New(str(o["error"]))
			}
		}
		return nil, err
	}
	if len(b) == 0 {
		return Object{}, nil
	}
	return decode(b)
}
func durationSeconds(v any, d int) time.Duration {
	n, err := toInt(v)
	if err != nil || n <= 0 {
		n = d
	}
	// A large but valid integer seconds count must fall back to the default
	// instead of overflowing time.Duration.
	if int64(n) > int64(math.MaxInt64/int64(time.Second)) {
		n = d
	}
	return time.Duration(n) * time.Second
}

// toInt converts an exact integer to a machine int. Typed integers convert
// exactly (never through float64, which would turn MaxInt64 into 1<<63 and
// then into MinInt). Values beyond the supported conversion bounds
// (including uint64 seeds above MaxInt64) are rejected instead of silently
// overflowing; the full uint64 range stays legal in generic helpers and the
// image path.
func toInt(v any) (int, error) {
	if !integer(v) {
		return 0, fail("Expected a whole number.")
	}
	switch x := v.(type) {
	case int:
		return x, nil
	case int8:
		return int(x), nil
	case int16:
		return int(x), nil
	case int32:
		return int(x), nil
	case int64:
		if x < math.MinInt || x > math.MaxInt {
			return 0, fail("Number is out of range for a machine integer.")
		}
		return int(x), nil
	case uint:
		if uint64(x) > math.MaxInt64 {
			return 0, fail("Number is too large for a machine integer.")
		}
		return int(x), nil
	case uint8:
		return int(x), nil
	case uint16:
		return int(x), nil
	case uint32:
		return int(x), nil
	case uint64:
		if x > math.MaxInt64 {
			return 0, fail("Number is too large for a machine integer.")
		}
		return int(x), nil
	case json.Number:
		if u, err := strconv.ParseUint(x.String(), 10, 64); err == nil {
			if u > math.MaxInt64 {
				return 0, fail("Number is too large for a machine integer.")
			}
			return int(u), nil
		}
		n, err := strconv.ParseInt(x.String(), 10, 64)
		if err != nil {
			return 0, fail("Number is out of range for a machine integer.")
		}
		return int(n), nil
	}
	// Only float64/float32 remain here. Values above MaxInt64 round up to
	// exactly 1<<63 in float64 and must be rejected, never truncated to
	// MinInt.
	n := num(v)
	if math.IsNaN(n) || n >= 9223372036854775808.0 || n < -9223372036854775808.0 {
		return 0, fail("Number is out of range for a machine integer.")
	}
	return int(n), nil
}

// intText preserves the exact spelling of integer and json.Number values;
// large IDs and budgets are never routed through float64.
func intText(v any) string {
	switch x := v.(type) {
	case json.Number:
		return x.String()
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	}
	return strconv.FormatFloat(num(v), 'f', -1, 64)
}
