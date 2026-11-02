package sparky

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Runtime contains device effects. The coordinator owns admission decisions.
type Runtime struct {
	mu        sync.Mutex
	Config    Object
	Path      string
	Run       Runner
	ProbeNode func(Object) (Object, error)
	// DrainAdmission fences new reservations and waits for direct Sparky
	// leases to drain before the legacy manager is reloaded. It returns the
	// restore function that clears the reload's own flags; the coordinator
	// owns admission so the fence can never orphan a flag on error. Nil in
	// unit seams.
	DrainAdmission func(ctx context.Context) (func(), error)
	// owner is the coordinator that wired DrainAdmission; nil in bare seams.
	owner   *Coordinator
	cache   Object
	cacheAt time.Time
}

func newRuntime(c Object, path string) *Runtime {
	return &Runtime{Config: c, Path: path, Run: runCommand, ProbeNode: Probe}
}
func (r *Runtime) config() Object { r.mu.Lock(); defer r.mu.Unlock(); return copyObject(r.Config) }
func (r *Runtime) configure(c Object) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Config = copyObject(c)
	r.cache = nil
}
func (r *Runtime) invalidate()  { r.mu.Lock(); r.cache = nil; r.mu.Unlock() }
func (r *Runtime) native() bool { return str(r.config()["lifecycle"]) == "native" }
func (r *Runtime) root() string { return str(r.config()["root"]) }
func (r *Runtime) execute(ctx context.Context, args []string, input []byte, timeout time.Duration) (CommandResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return r.Run(ctx, args, input)
}
func (r *Runtime) runChecked(ctx context.Context, args []string, input []byte, timeout time.Duration) (string, error) {
	x, err := r.execute(ctx, args, input, timeout)
	if err != nil || x.Code != 0 {
		return "", fail("Device operation failed: %s. Check the local service log.", filepath.Base(args[0]))
	}
	return x.Out, nil
}
func (r *Runtime) backendHeaders() map[string]string {
	env, err := readEnv(filepath.Join(r.root(), "vllm/api.env"))
	if err != nil || env["VLLM_API_KEY"] == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + env["VLLM_API_KEY"]}
}
func (r *Runtime) legacyManager(ctx context.Context, path, method string) (Object, error) {
	headers := r.backendHeaders()
	if len(headers) == 0 {
		return nil, fail("Private manager credential is missing.")
	}
	url := str(r.config()["manager_url"]) + path
	if path == "/running" || path == "/v1/models" {
		return requestObject(ctx, method, url, nil, headers, 8*time.Second, false)
	}
	_, _, err := request(ctx, method, url, nil, headers, 2*time.Hour)
	return Object{}, err
}
func (r *Runtime) legacyRequests(ctx context.Context) ([]any, error) {
	// The installed schema (swaputil/events.go) declares InFlightRequestsEvent
	// with `requests,omitempty`: an inflight snapshot WITHOUT a requests key
	// is a VALID empty snapshot, not an unknown state. A present null, a
	// wrong-typed requests field, a malformed event or a non-object/string
	// data payload is unknown: the caller must never treat it as idle.
	unknown := fail("Cannot confirm active manager requests. No model was stopped.")
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", str(r.config()["manager_url"])+"/api/events", nil)
	if err != nil {
		return nil, err
	}
	for k, v := range r.backendHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := httpClient(8 * time.Second).Do(req)
	if err != nil {
		return nil, unknown
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, unknown
	}
	scan := bufio.NewScanner(resp.Body)
	scan.Buffer(make([]byte, 4096), 4*1024*1024)
	for count := 0; count < 200 && scan.Scan(); count++ {
		line := scan.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		e, err := decode([]byte(strings.TrimPrefix(line, "data:")))
		if err != nil {
			return nil, unknown
		}
		if str(e["type"]) != "inflight" {
			continue
		}
		var data Object
		switch d := e["data"].(type) {
		case Object:
			data = d
		case string:
			if data, err = decode([]byte(d)); err != nil {
				return nil, unknown
			}
		default:
			return nil, unknown
		}
		if str(data["operation"]) != "snapshot" {
			continue
		}
		raw, present := data["requests"]
		if !present {
			return []any{}, nil
		}
		reqs, ok := raw.([]any)
		if !ok {
			return nil, unknown
		}
		return reqs, nil
	}
	return nil, unknown
}
func (r *Runtime) legacyActivity(ctx context.Context) (map[string]int, error) {
	counts := map[string]int{}
	if r.native() {
		return counts, nil
	}
	rows, err := r.legacyRequests(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range rows {
		row, ok := v.(Object)
		if !ok {
			return nil, fail("Cannot confirm active manager requests. No model was stopped.")
		}
		name := str(row["model"])
		if name == "" {
			name = str(row["model_id"])
		}
		// An unknown mapping ({}, model: null or a non-string without a
		// model_id) is never treated as idle; only a valid omitempty empty
		// snapshot is empty, and that is handled by legacyRequests.
		if name == "" {
			return nil, fail("Cannot confirm active manager requests. No model was stopped.")
		}
		counts[name]++
	}
	return counts, nil
}
func (r *Runtime) cancelLegacy(ctx context.Context, names []string) error {
	if r.native() {
		return nil
	}
	rows, err := r.legacyRequests(ctx)
	if err != nil {
		return err
	}
	for _, v := range rows {
		row, ok := v.(Object)
		if !ok {
			return fail("Invalid manager request entry.")
		}
		name := str(row["model"])
		if name == "" {
			name = str(row["model_id"])
		}
		if !contains(names, name) {
			continue
		}
		id := str(row["id"])
		if id == "" {
			id = intText(row["id"])
		}
		if !simpleName.MatchString(id) {
			return fail("Invalid manager request ID.")
		}
		_, status, err := request(ctx, "POST", str(r.config()["manager_url"])+"/api/inflight/"+id+"/cancel", nil, r.backendHeaders(), 8*time.Second)
		if err != nil && status != 404 {
			return fail("The manager could not cancel a conflicting request.")
		}
	}
	return nil
}
func (r *Runtime) inspectNode(ctx context.Context, name string) (Object, error) {
	c := r.config()
	meta := copyObject(obj(obj(c["nodes"])[name]))
	units := Object{}
	for n, v := range obj(c["models"]) {
		m := obj(v)
		if str(m["kind"]) == "llm" {
			units[n] = modelUnits(m, name)
		}
	}
	meta["units"] = units
	if name == "spark01" {
		return r.ProbeNode(meta)
	}
	host := str(meta["ssh"])
	if err := safeHost(host); err != nil {
		return nil, err
	}
	out, err := r.runChecked(ctx, []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", host, "~/.local/bin/sparky --json node probe --file -"}, jsonBytes(meta), 35*time.Second)
	if err != nil {
		return nil, err
	}
	return decode([]byte(out))
}
func (r *Runtime) modelStates(ctx context.Context) (map[string]string, error) {
	states := map[string]string{}
	if !r.native() {
		o, err := r.legacyManager(ctx, "/running", "GET")
		if err != nil {
			return nil, err
		}
		for _, v := range arr(o["running"]) {
			row := obj(v)
			states[str(row["model"])] = str(row["state"])
		}
		return states, nil
	}
	// Native: the durable ownership scan is authoritative for tracked
	// models. A corrupt, symlinked or inconsistent record is an error —
	// never a free fallback that would let work reuse a reserved GPU.
	own, err := r.nativeOwnership(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range enabledLLMs(r.config()) {
		row, ok := own[name]
		if !ok {
			continue // no durable record: not lifecycle-tracked
		}
		state := str(obj(row)["state"])
		if state == "ready" || state == "starting" {
			active, err := r.supervisorActive(ctx, name)
			if err != nil {
				return nil, err
			}
			if !active {
				state = "failed"
			}
		}
		states[name] = state
	}
	// Orphan records (deleted/disabled models and old placements) keep
	// their durable state visible; a corrupt or unknown record above is an
	// error, never a free fallback.
	for name, v := range own {
		if _, ok := states[name]; !ok {
			states[name] = str(obj(v)["state"])
		}
	}
	return states, nil
}
func (r *Runtime) snapshot(ctx context.Context, fresh bool) (Object, error) {
	r.mu.Lock()
	if !fresh && r.cache != nil && time.Since(r.cacheAt) < 2*time.Second {
		o := copyObject(r.cache)
		r.mu.Unlock()
		return o, nil
	}
	r.mu.Unlock()
	type item struct {
		name string
		data Object
		err  error
	}
	ch := make(chan item, 2)
	for _, name := range []string{"spark01", "spark02"} {
		go func(n string) { d, e := r.inspectNode(ctx, n); ch <- item{n, d, e} }(name)
	}
	nodes := Object{}
	for range 2 {
		x := <-ch
		if x.err != nil {
			return nil, x.err
		}
		nodes[x.name] = x.data
	}
	states, err := r.modelStates(ctx)
	if err != nil {
		return nil, err
	}
	// Durable native ownership: every non-stopped record — including
	// queued/starting/failed models, deleted or disabled models and old
	// placements — holds its recorded nodes as a RESERVATION. Physical
	// GPU/RSS stays in the node probes above; the merge is a UNION so an
	// actually-probed loaded node is never overwritten by a stopped row.
	own := Object{}
	if r.native() {
		own, err = r.nativeOwnership(ctx)
		if err != nil {
			return nil, err
		}
	}
	c := r.config()
	models := Object{}
	comfy := Object{}
	for name, data := range nodes {
		comfy[name] = obj(data)["comfyui"]
	}
	for name, v := range obj(c["models"]) {
		m := obj(v)
		kind := str(m["kind"])
		loadedNodes := []string{}
		loaded := false
		switch kind {
		case "llm":
			for n, v := range nodes {
				for _, u := range modelUnits(m, n) {
					if contains([]string{"active", "activating", "deactivating"}, str(obj(obj(v)["units"])[u])) {
						loadedNodes = append(loadedNodes, n)
						break
					}
				}
			}
			loaded = len(loadedNodes) > 0
		case "audio":
			on := str(m["run_on"])
			s := obj(obj(m["settings"])[on])
			loaded = true
			for _, n := range stringsOf(s["containers"]) {
				if !boolean(obj(obj(obj(nodes[on])["containers"])[n])["running"]) {
					loaded = false
				}
			}
			if loaded {
				loadedNodes = append(loadedNodes, on)
			}
		case "comfyui":
			for n, d := range comfy {
				if boolean(obj(d)["reachable"]) {
					loadedNodes = append(loadedNodes, n)
				}
			}
			loaded = len(loadedNodes) > 0
		}
		loadedNodes = unionNodes(loadedNodes, nil)
		state := states[name]
		if state == "" {
			state = "stopped"
			if loaded {
				state = "running"
			}
		}
		models[name] = Object{"kind": kind, "loaded": loaded, "nodes": loadedNodes, "desired": m["run_on"], "state": state, "keep_loaded": boolean(m["keep_loaded"]), "active_requests": 0}
	}
	// Merge the durable ownership rows in: reserved records hold their
	// recorded nodes even when the physical probe sees nothing (no rank
	// started yet, supervisor restarting) and orphan rows appear for models
	// no longer in the current config. Rows carry no recipe or private
	// record fields — only public wire metadata.
	for name, v := range own {
		row := obj(v)
		if !boolean(row["reserved"]) {
			continue // a stopped record never overwrites actual known state
		}
		recorded := stringsOf(row["nodes"])
		if existing, ok := models[name]; ok {
			m := obj(existing)
			if str(m["kind"]) != "llm" {
				continue // only llm records own rank nodes
			}
			m["nodes"] = unionNodes(stringsOf(m["nodes"]), recorded)
			m["loaded"] = true
			m["reserved"] = true
			m["keep_loaded"] = row["keep_loaded"]
			models[name] = m
			continue
		}
		models[name] = Object{"kind": "llm", "loaded": true, "reserved": true,
			"nodes": unionNodes(nil, recorded), "desired": nil,
			"state": str(row["state"]), "keep_loaded": row["keep_loaded"],
			"active_requests": 0}
	}
	out := Object{"nodes": nodes, "models": models, "comfyui": comfy}
	r.mu.Lock()
	r.cache = copyObject(out)
	r.cacheAt = time.Now()
	r.mu.Unlock()
	return out, nil
}
func (r *Runtime) unit(ctx context.Context, node, action, unit string) error {
	if !serviceName.MatchString(unit) || !contains([]string{"start", "stop", "restart"}, action) {
		return fail("Invalid service operation.")
	}
	args := []string{"sudo", "-n", "systemctl", action, unit}
	if node != "spark01" {
		h := str(obj(obj(r.config()["nodes"])[node])["ssh"])
		if err := safeHost(h); err != nil {
			return err
		}
		args = []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", h, shellJoin(args)}
	}
	_, err := r.runChecked(ctx, args, nil, 30*time.Minute)
	r.invalidate()
	return err
}
func (r *Runtime) clearComfy(ctx context.Context, node string) error {
	meta := obj(obj(r.config()["nodes"])[node])
	url := str(meta["comfy_url"])
	state, err := requestObject(ctx, "GET", url+"/sparky/state", nil, nil, 5*time.Second, false)
	if err != nil {
		probe, e := r.inspectNode(ctx, node)
		if e == nil && contains([]string{"inactive", "failed"}, str(obj(probe["units"])[str(meta["comfy_unit"])])) {
			return nil
		}
		return fail("Cannot confirm that ComfyUI is idle. No model was started.")
	}
	if str(state["phase"]) == "executing" {
		return fail("ComfyUI is executing a job. Cache was kept.")
	}
	_, err = requestObject(ctx, "POST", url+"/sparky/free", Object{}, nil, time.Minute, false)
	return err
}
func (r *Runtime) ensureAudio(ctx context.Context, name string) error {
	_, s, nodes, _, err := selectPlacement(r.config(), name, "")
	if err != nil {
		return err
	}
	for _, node := range nodes {
		args := append([]string{"sudo", "-n", "docker", "start"}, stringsOf(s["containers"])...)
		if node != "spark01" {
			args = []string{"ssh", str(obj(obj(r.config()["nodes"])[node])["ssh"]), shellJoin(args)}
		}
		if _, err := r.runChecked(ctx, args, nil, time.Minute); err != nil {
			return err
		}
	}
	return nil
}
func (r *Runtime) event(action string, fields Object) error {
	entry := merge(fields, Object{"time": now(), "action": action})
	path := filepath.Join(r.root(), "controller/events.jsonl")
	return withFileLock(path+".lock", true, func() error {
		f, e := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		defer f.Close()
		b, e := json.Marshal(entry)
		if e != nil {
			return e
		}
		_, e = f.Write(append(b, '\n'))
		return e
	})
}
func (r *Runtime) recordPath(name string) string {
	return filepath.Join(r.root(), "controller/lifecycle", name+".json")
}
func (r *Runtime) supervisorActive(ctx context.Context, name string) (bool, error) {
	state, err := r.supervisorState(ctx, name)
	if err != nil {
		return false, err
	}
	return contains([]string{"active", "activating"}, state), nil
}

// supervisorState queries the exact systemd ActiveState of a model
// supervisor. Strong readiness uses the exact value ("active" only);
// startup adoption may separately accept "activating".
func (r *Runtime) supervisorState(ctx context.Context, name string) (string, error) {
	x, e := r.execute(ctx, []string{"systemctl", "show", "sparky-model@" + name + ".service", "-p", "ActiveState", "--value"}, nil, 8*time.Second)
	if e != nil || x.Code != 0 {
		return "", fail("Cannot confirm model supervisor state.")
	}
	return strings.TrimSpace(x.Out), nil
}

// readEventSnapshot only consumes the bounded initial event; it never buffers an SSE stream to EOF.
var _ io.Reader
