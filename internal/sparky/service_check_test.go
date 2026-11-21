package sparky

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDoctorServiceRun injects a canned SSH runner for the service checks:
// the worker node reports serve mappings for OCR/speech/transcription, and
// the gateway host reports a healthy spark_ocr MCP client matching the OCR
// serve URL. No real SSH is attempted.
func fakeDoctorServiceRun(t *testing.T) {
	t.Helper()
	orig := doctorServiceRun
	doctorServiceRun = func(ctx context.Context, argv []string, _ []byte, _ func([]byte)) (remoteRunResult, error) {
		host := argv[5]
		switch host {
		case "spark02":
			return remoteRunResult{code: 0, stdout: []byte(`https://spark02.tail0a9712.ts.net (tailnet only)
|-- / proxy http://127.0.0.1:5092

https://spark02.tail0a9712.ts.net:8446 (tailnet only)
|-- / proxy http://127.0.0.1:8097

https://spark02.tail0a9712.ts.net:8445 (tailnet only)
|-- / proxy http://127.0.0.1:8096
`)}, nil
		case "spark01":
			return remoteRunResult{code: 0, stdout: []byte(`{"name":"spark_ocr","state":"healthy","disabled":false,"connection_string":"https://spark02.tail0a9712.ts.net:8446/mcp/"}`)}, nil
		}
		return remoteRunResult{code: 1, stderr: []byte("unexpected host")}, nil
	}
	t.Cleanup(func() { doctorServiceRun = orig })
}

func TestParseServeStatus(t *testing.T) {
	out := `https://spark02.tail0a9712.ts.net (tailnet only)
|-- / proxy http://127.0.0.1:5092

https://spark02.tail0a9712.ts.net:8443 (tailnet only)
|-- / proxy http://127.0.0.1:5093

https://spark02.tail0a9712.ts.net:8446 (tailnet only)
|-- / proxy http://127.0.0.1:8097

https://spark01.tail0a9712.ts.net (tailnet only)
|-- / proxy http://100.117.252.74:8000
`
	mappings := parseServeStatus(out)
	if len(mappings) != 4 {
		t.Fatalf("expected 4 mappings, got %v", mappings)
	}
	byOrigin := map[string]string{}
	for _, m := range mappings {
		byOrigin[m.origin] = m.target
	}
	if byOrigin["https://spark02.tail0a9712.ts.net"] != "http://127.0.0.1:5092" {
		t.Errorf("unexpected target for the root listener: %s", byOrigin["https://spark02.tail0a9712.ts.net"])
	}
	if byOrigin["https://spark02.tail0a9712.ts.net:8446"] != "http://127.0.0.1:8097" {
		t.Errorf("unexpected target for the 8446 listener: %s", byOrigin["https://spark02.tail0a9712.ts.net:8446"])
	}
	if byOrigin["https://spark02.tail0a9712.ts.net:8443"] != "http://127.0.0.1:5093" {
		t.Errorf("unexpected target for the 8443 listener: %s", byOrigin["https://spark02.tail0a9712.ts.net:8443"])
	}
	if byOrigin["https://spark01.tail0a9712.ts.net"] != "http://100.117.252.74:8000" {
		t.Errorf("unexpected target for the spark01 listener: %s", byOrigin["https://spark01.tail0a9712.ts.net"])
	}
	// A listener with no proxy line is never guessed.
	if got := parseServeStatus("https://example.ts.net (tailnet only)\n"); len(got) != 0 {
		t.Errorf("expected no mappings for a listener without a proxy line, got %v", got)
	}
}

func TestDoctorServiceChecks(t *testing.T) {
	fakeDoctorServiceRun(t)
	cfg := clientConfig{Version: 1, Gateway: "https://spark.example:8443",
		Extra: Object{"worker_ssh_host": "spark02", "admin_ssh_host": "spark01", "remote_root": "/home/ajaykumarns/spark-cluster"}}
	checks := doctorServiceChecks(context.Background(), cfg)
	if len(checks) != 4 {
		t.Fatalf("expected 4 service checks, got %v", checks)
	}
	byService := map[string]Object{}
	for _, c := range checks {
		byService[str(c["service"])] = c
	}
	for _, svc := range []string{"ocr", "speech", "transcription", "ocr-mcp"} {
		if _, ok := byService[svc]; !ok {
			t.Errorf("missing service check %s in %v", svc, checks)
		}
	}
	if str(byService["ocr"]["status"]) != "verified" {
		t.Errorf("ocr mapping check failed: %v", byService["ocr"])
	}
	if str(byService["speech"]["status"]) != "verified" {
		t.Errorf("speech mapping check failed: %v", byService["speech"])
	}
	if str(byService["transcription"]["status"]) != "verified" {
		t.Errorf("transcription mapping check failed: %v", byService["transcription"])
	}
	if str(byService["ocr-mcp"]["status"]) != "verified" {
		t.Errorf("ocr-mcp check failed: %v", byService["ocr-mcp"])
	}
}

func TestDoctorServiceChecksReportMissingServeMapping(t *testing.T) {
	orig := doctorServiceRun
	doctorServiceRun = func(ctx context.Context, argv []string, _ []byte, _ func([]byte)) (remoteRunResult, error) {
		return remoteRunResult{code: 0, stdout: []byte(`https://spark02.tail0a9712.ts.net (tailnet only)
|-- / proxy http://127.0.0.1:5092
`)}, nil
	}
	t.Cleanup(func() { doctorServiceRun = orig })
	cfg := clientConfig{Version: 1, Gateway: "https://spark.example:8443",
		Extra: Object{"worker_ssh_host": "spark02", "admin_ssh_host": "spark01", "remote_root": "/srv"}}
	checks := doctorServiceChecks(context.Background(), cfg)
	failed := map[string]bool{}
	for _, c := range checks {
		if str(c["status"]) == "failed" {
			failed[str(c["service"])] = true
		}
	}
	for _, svc := range []string{"ocr", "speech"} {
		if !failed[svc] {
			t.Errorf("missing mapping for %s must be reported failed: %v", svc, checks)
		}
	}
}

func TestMCPConnectionCheckReportsMismatch(t *testing.T) {
	orig := doctorServiceRun
	doctorServiceRun = func(ctx context.Context, argv []string, _ []byte, _ func([]byte)) (remoteRunResult, error) {
		switch argv[5] {
		case "spark02":
			return remoteRunResult{code: 0, stdout: []byte(`https://spark02.tail0a9712.ts.net:8446 (tailnet only)
|-- / proxy http://127.0.0.1:8097
`)}, nil
		case "spark01":
			// The stale connection string that the live gateway actually holds.
			return remoteRunResult{code: 0, stdout: []byte(`{"name":"spark_ocr","state":"unstable","disabled":false,"connection_string":"http://127.0.0.1:8097/mcp/"}`)}, nil
		}
		return remoteRunResult{code: 1, stderr: []byte("unexpected")}, nil
	}
	t.Cleanup(func() { doctorServiceRun = orig })
	cfg := clientConfig{Version: 1, Gateway: "https://spark.example:8443",
		Extra: Object{"worker_ssh_host": "spark02", "admin_ssh_host": "spark01", "remote_root": "/srv"}}
	checks := doctorServiceChecks(context.Background(), cfg)
	var mcp Object
	for _, c := range checks {
		if str(c["service"]) == "ocr-mcp" {
			mcp = c
		}
	}
	if mcp == nil {
		t.Fatalf("no ocr-mcp check: %v", checks)
	}
	if str(mcp["status"]) != "failed" {
		t.Fatalf("stale MCP connection string must fail: %v", mcp)
	}
	if !strings.Contains(str(mcp["detail"]), "http://127.0.0.1:8097/mcp/") {
		t.Fatalf("failure detail lost: %v", mcp)
	}
}

func TestMCPConnectionCheckSkipsWithoutAdminHost(t *testing.T) {
	orig := doctorServiceRun
	doctorServiceRun = func(ctx context.Context, argv []string, _ []byte, _ func([]byte)) (remoteRunResult, error) {
		if argv[5] == "spark01" {
			t.Error("no SSH to the admin host without an admin host configured")
			return remoteRunResult{code: 1}, nil
		}
		// The worker serve check runs as expected.
		return remoteRunResult{code: 0, stdout: []byte(`https://spark02.tail0a9712.ts.net:8446 (tailnet only)
|-- / proxy http://127.0.0.1:8097
`)}, nil
	}
	t.Cleanup(func() { doctorServiceRun = orig })
	cfg := clientConfig{Version: 1, Gateway: "https://spark.example:8443",
		Extra: Object{"worker_ssh_host": "spark02"}}
	checks := doctorServiceChecks(context.Background(), cfg)
	for _, c := range checks {
		if str(c["service"]) == "ocr-mcp" {
			t.Fatalf("MCP check must be skipped without an admin host: %v", c)
		}
	}
}

func TestDoctorServiceChecksSSHFailureIsHonest(t *testing.T) {
	orig := doctorServiceRun
	doctorServiceRun = func(ctx context.Context, argv []string, _ []byte, _ func([]byte)) (remoteRunResult, error) {
		return remoteRunResult{}, context.DeadlineExceeded
	}
	t.Cleanup(func() { doctorServiceRun = orig })
	cfg := clientConfig{Version: 1, Gateway: "https://spark.example:8443",
		Extra: Object{"worker_ssh_host": "spark02", "admin_ssh_host": "spark01", "remote_root": "/srv"}}
	checks := doctorServiceChecks(context.Background(), cfg)
	if len(checks) != 1 || str(checks[0]["service"]) != "serve" || str(checks[0]["status"]) != "failed" {
		t.Fatalf("unreachable worker host must report a failed serve check: %v", checks)
	}
}

func TestDoctorServiceChecksNoWorkerHost(t *testing.T) {
	cfg := clientConfig{Version: 1, Gateway: "https://spark.example:8443", Extra: Object{}}
	checks := doctorServiceChecks(context.Background(), cfg)
	if len(checks) != 1 || str(checks[0]["service"]) != "serve" || str(checks[0]["status"]) != "not_verified" {
		t.Fatalf("missing worker host must report not_verified: %v", checks)
	}
}

// TestDoctorJSONCarriesServiceChecks exercises the full CLI path with the
// fake SSH runner and the catalog gateway, so the wiring is covered end to end.
func TestDoctorJSONCarriesServiceChecks(t *testing.T) {
	dir := mainDirs(t)
	server := mainCatalogGateway(t)
	writeMainConfig(t, server.URL)
	t.Setenv("PATH", filepath.Join(dir, "empty-bin"))
	t.Setenv("HOME", dir)
	fakeDoctorServiceRun(t)
	setSmoke(t, ocrSmokeOK)
	code, out, _ := runMain([]string{"doctor", "--json"}, "")
	if code != 1 { // no harness binaries are installed here
		t.Fatalf("doctor must still exit 1 with harnesses missing: %d %q", code, out)
	}
	var data Object
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatalf("doctor json: %v %q", err, out)
	}
	serviceChecks := arr(data["service_checks"])
	if len(serviceChecks) != 4 {
		t.Fatalf("expected 4 service checks, got %v", serviceChecks)
	}
	if arr(data["checks"]) == nil {
		t.Fatal("checks list disappeared")
	}
}

func TestCanaryJSONShape(t *testing.T) {
	dir := mainDirs(t)
	server := mainCatalogGateway(t)
	writeMainConfig(t, server.URL)
	t.Setenv("HOME", dir)
	setSmoke(t, ocrSmokeOK)
	code, out, _ := runMain([]string{"canary", "--json"}, "")
	if code != 0 {
		t.Fatalf("canary code: %d %q", code, out)
	}
	var data Object
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatalf("canary json: %v %q", err, out)
	}
	if data["ok"] != true || data["inference"] != false {
		t.Fatalf("canary shape: %v", data)
	}
}

func TestCanaryHumanQuietOnSuccess(t *testing.T) {
	dir := mainDirs(t)
	server := mainCatalogGateway(t)
	writeMainConfig(t, server.URL)
	t.Setenv("HOME", dir)
	setSmoke(t, ocrSmokeOK)
	code, out, errOut := runMain([]string{"canary"}, "")
	if code != 0 {
		t.Fatalf("canary code: %d %q %q", code, out, errOut)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("canary output must be empty on success: %q", out)
	}
}

func TestCanaryHumanFailsLoudly(t *testing.T) {
	dir := mainDirs(t)
	server := mainCatalogGateway(t)
	writeMainConfig(t, server.URL)
	t.Setenv("HOME", dir)
	setSmoke(t, ocrSmokeErr)
	code, out, _ := runMain([]string{"canary"}, "")
	if code != 1 {
		t.Fatalf("canary code: %d %q", code, out)
	}
	if !strings.Contains(out, "Canary failed") {
		t.Fatalf("canary failure output lost: %q", out)
	}
}

func TestCanaryJSONFailsLoudly(t *testing.T) {
	dir := mainDirs(t)
	server := mainCatalogGateway(t)
	writeMainConfig(t, server.URL)
	t.Setenv("HOME", dir)
	setSmoke(t, ocrSmokeErr)
	code, out, _ := runMain([]string{"canary", "--json"}, "")
	if code != 1 {
		t.Fatalf("canary code: %d %q", code, out)
	}
	var data Object
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatalf("canary json: %v %q", err, out)
	}
	if data["ok"] != false {
		t.Fatalf("canary json shape: %v", data)
	}
}
