// Service-URL checks: the per-node service-URL catalog and the read-only
// doctor checks that inspect the gateway's service registration and the
// tailscale-serve mappings the services are exposed through. These checks
// never infer anything; they only read state (SSH probes and the pinned
// gateway admin API) and report what the cluster actually holds.
package sparky

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// serviceCatalogSpec describes one verifiable capability: the local port the
// service binds on its node, the gateway route exercised by the profile
// smoke, and the MCP endpoint path the client proxies to. Ports are the
// cluster's current documented layout (docs/profiles.md); if the layout
// drifts, these checks report it loudly instead of silently accepting it.
type serviceCatalogSpec struct {
	capability string
	localPort  int
	routePath  string
	mcpPath    string
}

var serviceCatalog = map[string]serviceCatalogSpec{
	"ocr":           {"ocr", 8097, "/v1/ocr", "/mcp/"},
	"speech":        {"speech", 8096, "/v1/audio/speech", ""},
	"transcription": {"transcription", 5092, "/v1/audio/transcriptions", ""},
}

// doctorServiceRun is the injectable seam for the doctor's read-only service
// checks. It is the same seam the client's remote protocol uses; tests
// inject it with fakes and never open real connections.
var doctorServiceRun remoteRunner = remoteRun

// doctorServiceTimeout bounds one read-only service check.
const doctorServiceTimeout = 30 * time.Second

// serveMapping is one tailscale-serve listener and the local target it
// proxies to.
type serveMapping struct {
	origin string // e.g. https://spark02.tail0a9712.ts.net:8446
	target string // e.g. http://127.0.0.1:8097
}

var (
	serveListenerRe = regexp.MustCompile(`^(https://[^\s(]+)`)
	serveProxyRe    = regexp.MustCompile(`^\|-- / proxy (\S+)$`)
)

// parseServeStatus parses `tailscale serve status` output: a listener line
// such as `https://example.tailnet:8446 (tailnet only)` followed by a
// `|-- / proxy <target>` line. Only listeners with a proxy line form a
// mapping (unmatched listeners are skipped, never guessed).
func parseServeStatus(out string) []serveMapping {
	var mappings []serveMapping
	pending := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if m := serveListenerRe.FindStringSubmatch(line); m != nil {
			pending = strings.TrimSuffix(m[1], "/")
			continue
		}
		if pending == "" {
			continue
		}
		if m := serveProxyRe.FindStringSubmatch(line); m != nil {
			target := m[1]
			if u, err := url.Parse(target); err == nil && u.Scheme != "" && u.Port() != "" {
				mappings = append(mappings, serveMapping{origin: pending, target: target})
			}
			pending = ""
		}
	}
	return mappings
}

// doctorServiceChecks runs the read-only per-node service checks: one
// tailscale-serve mapping per catalog service on the worker node, and the
// spark_ocr MCP client's connection string against the serve URL. No check
// infers anything; each reports what the cluster actually holds.
func doctorServiceChecks(ctx context.Context, cfg clientConfig) []Object {
	worker := str(cfg.Extra["worker_ssh_host"])
	if worker == "" {
		// No worker host configured: the checks cannot run and must never
		// be reported as passed.
		return []Object{{"service": "serve", "status": "not_verified",
			"detail": "no worker_ssh_host configured; the tailscale-serve checks cannot run"}}
	}
	if err := safeHost(worker); err != nil {
		return []Object{{"service": "serve", "status": "failed", "detail": err.Error()}}
	}
	serveOut, err := sshServiceCommand(ctx, worker, "tailscale serve status")
	if err != nil {
		return []Object{{"service": "serve", "status": "failed", "detail": err.Error()}}
	}
	mappings := parseServeStatus(serveOut)
	mappingByPort := map[int]serveMapping{}
	for _, m := range mappings {
		if u, err := url.Parse(m.target); err == nil && u.Port() != "" {
			port, err := strconv.Atoi(u.Port())
			if err == nil {
				mappingByPort[port] = m
			}
		}
	}
	checks := []Object{}
	for _, capability := range []string{"ocr", "speech", "transcription"} {
		spec := serviceCatalog[capability]
		mapping, found := mappingByPort[spec.localPort]
		if !found {
			checks = append(checks, Object{"service": capability, "status": "failed",
				"detail": fmt.Sprintf("no tailscale-serve mapping on the worker node for port %d (%s service)", spec.localPort, capability)})
			continue
		}
		checks = append(checks, Object{"service": capability, "status": "verified",
			"detail": fmt.Sprintf("%s serves %s", mapping.origin, mapping.target)})
	}
	if mcp := mcpConnectionCheck(ctx, cfg, mappingByPort[serviceCatalog["ocr"].localPort]); mcp != nil {
		checks = append(checks, *mcp)
	}
	return checks
}

// sshServiceCommand runs one pinned read-only command remotely through the
// bounded, injectable SSH seam. Errors carry no command output.
func sshServiceCommand(ctx context.Context, host, command string) (string, error) {
	if err := safeHost(host); err != nil {
		return "", err
	}
	argv := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", host, command}
	ctx, cancel := context.WithTimeout(ctx, doctorServiceTimeout)
	defer cancel()
	out, err := doctorServiceRun(ctx, argv, nil, nil)
	if err != nil {
		return "", fail("Cannot read the remote state on the service node.")
	}
	if strings.TrimSpace(string(out.stdout)) == "" {
		return "", fail("The service node returned no output.")
	}
	return string(out.stdout), nil
}

// mcpClientReport is the redacted, public-safe shape printed back from the
// gateway host. No headers, tokens, tools, or private fields are ever
// returned.
type mcpClientReport struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	Disabled   bool   `json:"disabled"`
	Connection string `json:"connection_string"`
}

// mcpCheckScript is the pinned python one-liner run over SSH on the gateway
// host. The gateway.env credentials stay on the host; the script prints
// only the name, state, disabled flag, and connection string of the spark_ocr
// client — never headers, tokens, tools, or private config.
const mcpCheckScriptTemplate = `import base64,json
from pathlib import Path
from urllib.request import Request,urlopen
env=dict(l.split('=',1) for l in Path(%s+'/gateway/gateway.env').read_text().splitlines() if '=' in l)
auth='Basic '+base64.b64encode((env['BIFROST_ADMIN_USER']+':'+env['BIFROST_ADMIN_PASSWORD']).encode()).decode()
with urlopen(Request('http://127.0.0.1:8081/api/mcp/clients',headers={'Authorization':auth}),timeout=30) as r:
    data=json.load(r)
for c in data.get('clients', []):
    cfg=c.get('config',{})
    if cfg.get('name')=='spark_ocr':
        cs=cfg.get('connection_string')
        if isinstance(cs,dict): cs=cs.get('value')
        print(json.dumps({'name':cfg.get('name'),'state':c.get('state'),'disabled':bool(cfg.get('disabled')),'connection_string':cs}))
        break
`

// mcpConnectionCheck reports whether the spark_ocr MCP client connection
// string points at the actual OCR service served through Tailscale Serve.
// The expected URL comes from the serve mapping, so nothing is hardcoded.
func mcpConnectionCheck(ctx context.Context, cfg clientConfig, ocrServe serveMapping) *Object {
	admin := str(cfg.Extra["admin_ssh_host"])
	if admin == "" {
		return nil // The MCP check is only meaningful when admin access is configured.
	}
	if err := safeHost(admin); err != nil {
		out := Object{"service": "ocr-mcp", "status": "failed", "detail": err.Error()}
		return &out
	}
	root := str(cfg.Extra["remote_root"])
	if root == "" {
		return nil
	}
	if ocrServe.origin == "" {
		out := Object{"service": "ocr-mcp", "status": "not_verified",
			"detail": "cannot compare the MCP connection string: the OCR tailscale-serve mapping is missing or unreachable"}
		return &out
	}
	// Quote the root so the python one-liner cannot be broken out of by a
	// hostile or unexpected path — json.Marshal produces a quoted literal.
	rootLiteral, err := json.Marshal(root)
	if err != nil {
		outObj := Object{"service": "ocr-mcp", "status": "failed", "detail": "cannot build the gateway host check"}
		return &outObj
	}
	script := strings.Replace(mcpCheckScriptTemplate, "%s", string(rootLiteral), 1)
	out, err := sshServiceCommand(ctx, admin, "python3 -c "+shellJoin([]string{script}))
	if err != nil {
		outObj := Object{"service": "ocr-mcp", "status": "failed", "detail": err.Error()}
		return &outObj
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	report := mcpClientReport{}
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		outObj := Object{"service": "ocr-mcp", "status": "failed", "detail": "the gateway host returned no spark_ocr MCP client report"}
		return &outObj
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(lines[0])), &report); err != nil {
		outObj := Object{"service": "ocr-mcp", "status": "failed", "detail": "the gateway host returned an unreadable spark_ocr MCP client report"}
		return &outObj
	}
	// The connection string is a URL, never a secret. Compare its scheme,
	// host, port, and path against the served OCR URL.
	expected := strings.TrimSuffix(ocrServe.origin, "/") + serviceCatalog["ocr"].mcpPath
	got := strings.TrimSuffix(report.Connection, "/")
	if got == "" {
		outObj := Object{"service": "ocr-mcp", "status": "failed", "detail": "the spark_ocr MCP client has no connection string"}
		return &outObj
	}
	if got != strings.TrimSuffix(expected, "/") {
		outObj := Object{"service": "ocr-mcp", "status": "failed",
			"detail": fmt.Sprintf("the spark_ocr MCP client connection string %s does not match the OCR service URL %s", report.Connection, expected)}
		return &outObj
	}
	if report.Disabled {
		outObj := Object{"service": "ocr-mcp", "status": "failed", "detail": "the spark_ocr MCP client is disabled"}
		return &outObj
	}
	detail := "the spark_ocr MCP client serves"
	if report.State != "" {
		detail += " and reports state " + report.State
	}
	outObj := Object{"service": "ocr-mcp", "status": "verified", "detail": detail}
	return &outObj
}

// serviceCheckResult renders one row of the service-checks table.
func serviceCheckResult(check Object) (string, string, string) {
	service := str(check["service"])
	label := service
	if service == "ocr-mcp" {
		label = "OCR MCP"
	} else {
		label = profileLabel(service)
	}
	status := str(check["status"])
	result := "failed"
	if status == "verified" {
		result = "ok"
	}
	if status == "not_verified" {
		result = "note"
	}
	return label, result, strings.Join(strings.Fields(str(check["detail"])), " ")
}
