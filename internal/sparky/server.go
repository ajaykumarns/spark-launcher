package sparky

// Controller ingress. Mirrors src/spark_launcher/controller_server.py: the
// routes, privacy rules and error envelopes are preserved while the transport
// moves to net/http with streaming via httputil.
//
// Routes: GET /health, GET /v1/image-models, GET /v1/models, GET
// /sparky/catalog, POST /v1/images/generations, GET+POST /control/*, POST
// /jobs/acquire|release|cancel, and POST /v1/chat/completions plus
// /v1/completions for loopback inference. Responses and embeddings stay
// unsupported; Bifrost performs Responses translation.
//
// Privacy: the peer IP is taken from the connection (RemoteAddr). Forwarded
// headers are never trusted for administrator identity, job sources must
// match their device's direct link, and nothing about prompts, bodies,
// authorization headers or user URLs is logged. The native model listing
// carries capability/context/status metadata only, never backend URLs or
// tokens.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const controllerMaxBody = 128 << 20

// Controller is the narrow admission interface the server needs. The
// Coordinator (owned in a later task) implements it; ServeController wires
// the real implementation through CoordinatorFactory.
type Controller interface {
	ConfigPath() string
	Config() Object
	// Refresh re-reads the central cluster file; errors must surface as
	// HTTP envelopes, never as an empty successful state.
	Refresh() (Object, error)
	// Snapshot reports the device state; fresh forces a new device query.
	Snapshot(fresh bool) (Object, error)
	Admission() Object
	Fenced() bool
	Fence(enabled bool)
	Plan(model, action, on string) (Object, error)
	Load(model string, force bool, on string) (Object, error)
	Unload(model string, force bool) (Object, error)
	SetPlacement(model, on string) (Object, error)
	SubmitComfy(workflow Object, on string) (Object, error)
	// LLMRequest takes the request context so a disconnected client can
	// cancel a queued admission; the token is released by the server.
	LLMRequest(ctx context.Context, model string) (name string, token string, err error)
	InferenceOrigin(token string) (string, error)
	Release(token string)
	RegisterInferenceCancel(token string, cancel context.CancelFunc)
	ComfyAcquire(node, promptID string, cpuOnly bool) (Object, error)
	CancelJob(node, promptID string)
	CheckJobLease(token, node string) error
	ReconcileJobs()
	ImageModels() []any
	Generate(body Object) (Object, error)
	// LLMWire returns the Bifrost-required /v1/models listing: the installed
	// manager listing in legacy parity mode, or a synthesized native list.
	// Errors must surface as envelopes instead of an empty HTTP 200 list.
	LLMWire() (Object, error)
}

// CoordinatorFactory builds the real admission controller from a cluster
// config path. coordinator.go sets it once the Coordinator implements
// Controller.
var CoordinatorFactory func(configPath string) (Controller, error)

type controllerServer struct {
	ctrl Controller
}

// catalogHook lets tests inject a catalog; production always uses
// modelCatalog(config).
var catalogHook = modelCatalog

func (s *controllerServer) catalog() Object {
	if live, ok := s.ctrl.(interface{ LiveCatalog() Object }); ok {
		return live.LiveCatalog()
	}
	return catalogHook(s.ctrl.Config())
}

// ServeController binds the loopback and the configured Spark 1 direct link
// only, reconciles ComfyUI jobs periodically, and shuts down gracefully on
// SIGINT or SIGTERM. Model supervision runs in its own units and is
// untouched here. There is no server WriteTimeout: long inference streams
// must not be cut short. Every listener is bound synchronously; a bind
// failure closes the already opened listeners and returns an error instead
// of leaving a half-healthy controller behind.
func ServeController(configPath string) error {
	if CoordinatorFactory == nil {
		return fail("Controller is not linked; the coordinator must set CoordinatorFactory.")
	}
	ctrl, err := CoordinatorFactory(configPath)
	if err != nil {
		return err
	}
	s := &controllerServer{ctrl: ctrl}
	if routing, ok := ctrl.(interface{ ReconcileAutoRouting() error }); ok {
		_ = routing.ReconcileAutoRouting()
	}
	cfg := ctrl.Config()
	port := controllerPort(cfg)
	if port == 0 {
		return fail("The cluster file has no usable controller port.")
	}
	addresses := []string{"127.0.0.1", str(obj(obj(cfg["nodes"])["spark01"])["direct_ip"])}
	listeners := []*net.TCPListener{}
	var closeOnce sync.Once
	shutdown := func() {
		closeOnce.Do(func() {
			for _, listener := range listeners {
				_ = listener.Close()
			}
		})
	}
	defer shutdown()
	for _, address := range addresses {
		if address == "" {
			continue
		}
		listener, err := net.Listen("tcp", net.JoinHostPort(address, strconv.Itoa(port)))
		if err != nil {
			return fail("Could not bind the controller on %s. The port may already be in use: %v", address, err)
		}
		if tcp, ok := listener.(*net.TCPListener); ok {
			listeners = append(listeners, tcp)
		} else {
			_ = listener.Close()
		}
	}
	if len(listeners) == 0 {
		return fail("No controller bind address is configured.")
	}

	serveErr := make(chan error, len(listeners))
	servers := []*http.Server{}
	for _, listener := range listeners {
		server := &http.Server{Handler: s, ReadHeaderTimeout: 60 * time.Second}
		servers = append(servers, server)
		listener := listener
		go func() {
			if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
				serveErr <- err
			}
		}()
	}

	reconcileErr := make(chan error, 1)
	reconcileStop := make(chan struct{})
	defer close(reconcileStop)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-reconcileStop:
				return
			case <-ticker.C:
				func() {
					// A panic in reconciliation is an unexpected server
					// failure and must surface, not be swallowed; expected
					// errors are handled inside ReconcileJobs.
					defer func() {
						if recovered := recover(); recovered != nil {
							reconcileErr <- fail("The ComfyUI job reconciliation failed unexpectedly. See the local service journal.")
						}
					}()
					ctrl.ReconcileJobs()
					if routing, ok := ctrl.(interface{ ReconcileAutoRouting() error }); ok {
						_ = routing.ReconcileAutoRouting()
					}
				}()
			}
		}
	}()

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		shutdown()
		return fail("The controller listener failed: %v", err)
	case err := <-reconcileErr:
		shutdown()
		return err
	}
	// Graceful stop: bound the drain of in-flight responses, then close the
	// listeners. The deferred shutdown is a no-op if an error path ran.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDrain()
	for _, server := range servers {
		_ = server.Shutdown(drainCtx)
	}
	shutdown()
	return nil
}

// peerHost returns the verified peer IP of the connection. Forwarded headers
// are never consulted.
func peerHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *controllerServer) respond(w http.ResponseWriter, status int, data any) {
	body, err := json.Marshal(data)
	if err != nil {
		body = []byte(`{"error":"The response could not be encoded."}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *controllerServer) failRequest(w http.ResponseWriter, status int, message string) {
	s.respond(w, status, Object{"error": message})
}

// unavailable reports a failed config, device or manager query without
// leaking raw upstream diagnostics.
func (s *controllerServer) unavailable(w http.ResponseWriter, err error) {
	message := "A device operation failed. See the local service journal."
	if err != nil && err.Error() != "" {
		message = err.Error()
	}
	s.failRequest(w, http.StatusServiceUnavailable, message)
}

// jsonBody reads a strictly framed, size-bounded JSON object body. One
// decoder with UseNumber preserves the exact numeric spelling (including
// uint64 values such as 18446744073709551615) into downstream handlers, and
// a second decode must hit EOF so trailing or concatenated JSON is rejected.
// The read deadline is bounded only while the body is read.
func (s *controllerServer) jsonBody(w http.ResponseWriter, r *http.Request) (Object, bool) {
	if len(r.TransferEncoding) > 1 || (len(r.TransferEncoding) > 0 && r.TransferEncoding[0] != "chunked") {
		s.failRequest(w, http.StatusBadRequest, "Ambiguous request body framing.")
		return nil, false
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(60 * time.Second))
	defer func() { _ = controller.SetReadDeadline(time.Time{}) }() // clear after the body is read
	r.Body = http.MaxBytesReader(w, r.Body, controllerMaxBody)
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	var raw Object
	if err := decoder.Decode(&raw); err != nil {
		s.failRequest(w, http.StatusBadRequest, "The request body must be a JSON object within the transport size limit.")
		return nil, false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		s.failRequest(w, http.StatusBadRequest, "The request body must contain exactly one JSON object.")
		return nil, false
	}
	if !isObject(raw) {
		s.failRequest(w, http.StatusBadRequest, "The request body must be a JSON object.")
		return nil, false
	}
	return raw, true
}

// singleResponse guarantees at most one HTTP status per request even if a
// late error path or proxy callback tries to respond after streaming began.
type singleResponse struct {
	http.ResponseWriter
	wrote bool
}

func (s *singleResponse) WriteHeader(status int) {
	if s.wrote {
		return
	}
	s.wrote = true
	s.ResponseWriter.WriteHeader(status)
}

func (s *singleResponse) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

// Flush marks the headers committed and delegates to the real writer so
// ReverseProxy can stream the first chunk immediately.
func (s *singleResponse) Flush() {
	s.wrote = true
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap keeps http.ResponseController working through the wrapper.
func (s *singleResponse) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *controllerServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	guarded := &singleResponse{ResponseWriter: w}
	defer func() {
		if recovered := recover(); recovered != nil {
			// Once headers or body bytes are committed, appending an error
			// envelope would corrupt a live stream: abort the connection
			// instead. Only a genuinely uncommitted request gets the safe
			// envelope. No request data, headers or raw diagnostics are
			// ever written to the client.
			if guarded.wrote || recovered == http.ErrAbortHandler {
				panic(http.ErrAbortHandler)
			}
			s.failRequest(w, http.StatusServiceUnavailable, "A device operation failed. See the local service journal.")
		}
	}()
	w = guarded
	path := r.URL.Path
	switch {
	case path == "/health" && r.Method == http.MethodGet:
		s.respond(w, http.StatusOK, Object{"status": "ok"})
	case path == "/v1/image-models" && r.Method == http.MethodGet:
		if _, err := s.ctrl.Refresh(); err != nil {
			s.unavailable(w, err)
			return
		}
		s.respond(w, http.StatusOK, Object{"object": "list", "data": s.ctrl.ImageModels()})
	case path == "/v1/models" && r.Method == http.MethodGet:
		s.serveModels(w)
	case path == "/sparky/catalog" && r.Method == http.MethodGet:
		if _, err := s.ctrl.Refresh(); err != nil {
			s.unavailable(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		s.respond(w, http.StatusOK, s.catalog())
	case path == "/v1/images/generations" && r.Method == http.MethodPost:
		body, ok := s.jsonBody(w, r)
		if !ok {
			return
		}
		result, err := s.ctrl.Generate(body)
		if err != nil {
			status := http.StatusConflict
			var se interface{ HTTPStatus() int }
			if errors.As(err, &se) {
				status = se.HTTPStatus()
			}
			s.failRequest(w, status, err.Error())
			return
		}
		s.respond(w, http.StatusOK, result)
	case path == "/v1/chat/completions" || path == "/v1/completions":
		if r.Method != http.MethodPost {
			s.failRequest(w, http.StatusMethodNotAllowed, "Method not allowed.")
			return
		}
		s.serveInference(w, r, path)
	case strings.HasPrefix(path, "/control/"):
		s.serveControl(w, r, path)
	case path == "/jobs/acquire" || path == "/jobs/release" || path == "/jobs/cancel":
		if r.Method != http.MethodPost {
			s.failRequest(w, http.StatusMethodNotAllowed, "Method not allowed.")
			return
		}
		s.serveJobs(w, r, path)
	default:
		// Bifrost translates Responses into chat completions; no embedding
		// runtime or native Responses server is installed behind this proxy.
		s.failRequest(w, http.StatusNotFound, "Unknown endpoint")
	}
}

// serveModels returns the Bifrost-required wire listing with the central
// catalog metadata (capabilities, context, status) overlaid, plus the image
// models. A failed listing is an error envelope, never an empty HTTP 200.
func (s *controllerServer) serveModels(w http.ResponseWriter) {
	wire, err := s.ctrl.LLMWire()
	if err != nil {
		s.unavailable(w, err)
		return
	}
	if _, err := s.ctrl.Refresh(); err != nil {
		s.unavailable(w, err)
		return
	}
	if !isObject(wire) {
		wire = Object{}
	}
	catalog := s.catalog()
	extra := map[string]Object{}
	for _, row := range arr(catalog["data"]) {
		entry := obj(row)
		key := strings.TrimPrefix(str(entry["id"]), "garuda/")
		extra[key] = entry
	}
	rows := arr(wire["data"])
	overlaid := make([]any, 0, len(rows))
	for _, row := range rows {
		entry := obj(row)
		key := strings.TrimPrefix(str(entry["id"]), "garuda/")
		if extraRow, ok := extra[key]; ok {
			merged := copyObject(entry)
			for _, k := range keys(extraRow) {
				if k != "id" {
					merged[k] = extraRow[k]
				}
			}
			row = merged
		}
		overlaid = append(overlaid, row)
	}
	wire["data"] = append(overlaid, s.ctrl.ImageModels()...)
	s.respond(w, http.StatusOK, wire)
}

// /control/fence is both a read (GET, the installer's prior-state read and
// any loopback status check) and a write (POST, the manual toggle). Every
// other read and write path keeps its single verb.
var controlReads = map[string]bool{"/control/status": true, "/control/config": true, "/control/leases": true,
	"/control/fence": true}
var controlWrites = map[string]bool{"/control/fence": true, "/control/plan": true, "/control/load": true,
	"/control/unload": true, "/control/run": true, "/control/placement": true}

func (s *controllerServer) serveControl(w http.ResponseWriter, r *http.Request, path string) {
	// Administrator controls are loopback-only via SSH tunnels; the peer IP
	// from the connection is the only identity source.
	if !loopback(peerHost(r)) {
		s.failRequest(w, http.StatusForbidden, "Use administrator SSH for control operations.")
		return
	}
	// /control/fence accepts both the read and the write verb; every other
	// control path keeps exactly one allowed method.
	both := controlReads[path] && controlWrites[path]
	if controlReads[path] && !both && r.Method != http.MethodGet {
		s.failRequest(w, http.StatusMethodNotAllowed, "Method not allowed.")
		return
	}
	if controlWrites[path] && !both && r.Method != http.MethodPost {
		s.failRequest(w, http.StatusMethodNotAllowed, "Method not allowed.")
		return
	}
	if !controlReads[path] && !controlWrites[path] {
		s.failRequest(w, http.StatusNotFound, "Unknown endpoint")
		return
	}
	if both && r.Method != http.MethodGet && r.Method != http.MethodPost {
		s.failRequest(w, http.StatusMethodNotAllowed, "Method not allowed.")
		return
	}
	var body Object
	if r.Method == http.MethodPost {
		var ok bool
		body, ok = s.jsonBody(w, r)
		if !ok {
			return
		}
	}
	switch path {
	case "/control/fence":
		if r.Method == http.MethodGet {
			// Read-only fence state: the exact manual flag, no body.
			s.respond(w, http.StatusOK, Object{"fenced": s.ctrl.Fenced()})
			return
		}
		enabled, isBool := body["enabled"].(bool)
		if !isBool {
			s.failRequest(w, http.StatusBadRequest, "enabled must be true or false.")
			return
		}
		s.ctrl.Fence(enabled)
		s.respond(w, http.StatusOK, Object{"fenced": enabled})
	case "/control/status":
		snapshot, err := s.ctrl.Snapshot(true)
		if err != nil {
			s.unavailable(w, err)
			return
		}
		s.respond(w, http.StatusOK, snapshot)
	case "/control/config":
		config, err := s.ctrl.Refresh()
		if err != nil {
			s.unavailable(w, err)
			return
		}
		s.respond(w, http.StatusOK, Object{"path": s.ctrl.ConfigPath(), "config": config})
	case "/control/leases":
		s.respond(w, http.StatusOK, s.ctrl.Admission())
	case "/control/plan":
		model := str(body["model"])
		if model == "" {
			s.failRequest(w, http.StatusBadRequest, "model is required.")
			return
		}
		result, err := s.ctrl.Plan(model, str(value(body, "action", "load")), str(body["on"]))
		if err != nil {
			s.failRequest(w, http.StatusConflict, err.Error())
			return
		}
		s.respond(w, http.StatusOK, result)
	case "/control/load", "/control/unload":
		model := str(body["model"])
		if model == "" {
			s.failRequest(w, http.StatusBadRequest, "model is required.")
			return
		}
		force, isBool := value(body, "force", false).(bool)
		if !isBool {
			s.failRequest(w, http.StatusBadRequest, "force must be true or false.")
			return
		}
		var result Object
		var err error
		if path == "/control/load" {
			on := ""
			if raw, has := body["on"]; has && raw != nil {
				on = str(raw)
				if placements[on] == nil {
					s.failRequest(w, http.StatusBadRequest, "on must be spark01, spark02, or both.")
					return
				}
			}
			result, err = s.ctrl.Load(model, force, on)
		} else {
			if raw, has := body["on"]; has && raw != nil {
				s.failRequest(w, http.StatusBadRequest, "on is available only for load.")
				return
			}
			result, err = s.ctrl.Unload(model, force)
		}
		if err != nil {
			s.failRequest(w, http.StatusConflict, err.Error())
			return
		}
		s.respond(w, http.StatusOK, result)
	case "/control/run":
		if !isObject(body["workflow"]) {
			s.failRequest(w, http.StatusBadRequest, "workflow must be a JSON object.")
			return
		}
		result, err := s.ctrl.SubmitComfy(obj(body["workflow"]), str(body["on"]))
		if err != nil {
			s.failRequest(w, http.StatusConflict, err.Error())
			return
		}
		s.respond(w, http.StatusOK, result)
	case "/control/placement":
		model := str(body["model"])
		on := str(body["on"])
		if model == "" || on == "" {
			s.failRequest(w, http.StatusBadRequest, "model and on are required.")
			return
		}
		result, err := s.ctrl.SetPlacement(model, on)
		if err != nil {
			s.failRequest(w, http.StatusConflict, err.Error())
			return
		}
		s.respond(w, http.StatusOK, result)
	}
}

func (s *controllerServer) serveJobs(w http.ResponseWriter, r *http.Request, path string) {
	body, ok := s.jsonBody(w, r)
	if !ok {
		return
	}
	node := str(body["node"])
	nodes := obj(s.ctrl.Config()["nodes"])
	if _, known := nodes[node]; !known {
		s.failRequest(w, http.StatusBadRequest, "Unknown job device.")
		return
	}
	// A direct-node job source must match its own device; loopback is only
	// accepted for the head node itself.
	allowed := map[string]bool{str(obj(nodes[node])["direct_ip"]): true}
	if node == "spark01" {
		for _, host := range []string{"127.0.0.1", "::1"} {
			allowed[host] = true
		}
	}
	if !allowed[peerHost(r)] {
		s.failRequest(w, http.StatusForbidden, "Job source does not match its device.")
		return
	}
	switch path {
	case "/jobs/acquire":
		promptID := str(body["prompt_id"])
		if promptID == "" {
			s.failRequest(w, http.StatusBadRequest, "prompt_id is required.")
			return
		}
		if raw, present := body["cpu_only"]; present {
			if _, isBool := raw.(bool); !isBool {
				s.failRequest(w, http.StatusBadRequest, "cpu_only must be true or false.")
				return
			}
		}
		result, err := s.ctrl.ComfyAcquire(node, promptID, boolean(body["cpu_only"]))
		if err != nil {
			s.failRequest(w, http.StatusConflict, err.Error())
			return
		}
		s.respond(w, http.StatusOK, result)
	case "/jobs/cancel":
		promptID := str(body["prompt_id"])
		if promptID == "" {
			s.failRequest(w, http.StatusBadRequest, "prompt_id is required.")
			return
		}
		s.ctrl.CancelJob(node, promptID)
		s.respond(w, http.StatusOK, Object{"status": "cancelled"})
	case "/jobs/release":
		token := str(body["token"])
		if token == "" {
			s.failRequest(w, http.StatusBadRequest, "token is required.")
			return
		}
		if err := s.ctrl.CheckJobLease(token, node); err != nil {
			s.failRequest(w, http.StatusConflict, err.Error())
			return
		}
		s.ctrl.Release(token)
		s.respond(w, http.StatusOK, Object{"status": "released"})
	}
}

// serveInference proxies loopback inference straight to the reserved server.
// Admission is released exactly once even when the client disconnects while
// queued or the request is cancelled before the response headers arrive.
func (s *controllerServer) serveInference(w http.ResponseWriter, r *http.Request, path string) {
	if !loopback(peerHost(r)) {
		s.failRequest(w, http.StatusForbidden, "Use the existing gateway for inference.")
		return
	}
	body, ok := s.jsonBody(w, r)
	if !ok {
		return
	}
	model := str(body["model"])
	if model == "" {
		s.failRequest(w, http.StatusBadRequest, "model is required")
		return
	}
	// The request context travels into admission: a client disconnect while
	// queued must cancel the wait, not strand it in the FIFO.
	name, token, err := s.ctrl.LLMRequest(r.Context(), model)
	if err != nil {
		if r.Context().Err() != nil {
			return // the client is gone; no envelope can be delivered
		}
		status := http.StatusConflict
		var statusError interface{ HTTPStatus() int }
		if errors.As(err, &statusError) {
			status = statusError.HTTPStatus()
		}
		s.failRequest(w, status, err.Error())
		return
	}
	var released sync.Once
	release := func() { released.Do(func() { s.ctrl.Release(token) }) }
	defer release()
	body["model"] = name
	payload, err := json.Marshal(body)
	if err != nil {
		s.failRequest(w, http.StatusBadRequest, "The request body must be a JSON object.")
		return
	}
	origin, err := s.ctrl.InferenceOrigin(token)
	if err != nil {
		s.failRequest(w, http.StatusConflict, err.Error())
		return
	}
	target, err := url.Parse(origin)
	if err != nil || target.Host == "" {
		s.failRequest(w, http.StatusConflict, "The model has no usable inference origin.")
		return
	}
	// The configured origin is HTTP loopback or the direct link: exact
	// scheme and host, no userinfo, path, query or fragment.
	if target.Scheme != "http" || target.User != nil || strings.Trim(target.Path, "/") != "" || target.RawQuery != "" || target.Fragment != "" {
		s.failRequest(w, http.StatusConflict, "The model has no usable inference origin.")
		return
	}
	// Cancellation covers prefill before the response headers: the
	// coordinator may invoke the registered cancel while the upstream
	// request is still being written, and the client disconnect cancels the
	// derived context the same way.
	outCtx, cancel := context.WithCancel(r.Context())
	defer cancel()
	inCtx := r.Context() // the live incoming client context, independent of outCtx
	s.ctrl.RegisterInferenceCancel(token, cancel)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// The route is the configured origin with the inference path
			// only: the client's URL query is dropped and the exact scheme
			// and host from configuration are honored. ReverseProxy already
			// strips hop-by-hop headers and Connection token headers while
			// preserving the client's authentication headers.
			pr.SetURL(&url.URL{Scheme: target.Scheme, Host: target.Host})
			pr.Out.URL.Path = path
			pr.Out.URL.RawQuery = ""
			pr.Out.URL.RawPath = ""
			pr.Out.Host = target.Host
		},
		// Stream chunks to the client immediately; close-delimited streaming
		// avoids buffering long reasoning replies.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, proxyErr error) {
			// Only reachable before the response headers were written, so
			// exactly one response is emitted. A forced cancellation during
			// prefill must never fall through to net/http's default empty
			// 200, which would falsely indicate success: a still-connected
			// client gets a non-2xx cancellation envelope, a gone client
			// gets a connection abort with no write at all.
			if outCtx.Err() != nil {
				if inCtx.Err() != nil {
					panic(http.ErrAbortHandler)
				}
				s.failRequest(w, http.StatusBadGateway, "The inference request was cancelled.")
				return
			}
			s.failRequest(w, http.StatusBadGateway, "Model upstream is unavailable.")
		},
	}
	r.Body = io.NopCloser(bytes.NewReader(payload))
	r.ContentLength = int64(len(payload))
	r.Header.Set("Content-Length", strconv.Itoa(len(payload)))
	proxy.ServeHTTP(w, r.WithContext(outCtx))
}
