package sparky

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ocrSmokeOK returns a response shaped like the OCR gateway's success path.
func ocrSmokeOK(_ context.Context, _, _ string, _ any, _ map[string]string, _ time.Duration, _ bool) (Object, error) {
	return Object{"pages": []any{Object{"markdown": "hello"}}}, nil
}

// ocrSmokeErr is a canned transport failure.
func ocrSmokeErr(_ context.Context, _, _ string, _ any, _ map[string]string, _ time.Duration, _ bool) (Object, error) {
	return nil, errors.New("Cannot reach the server. Check Tailscale and the gateway.")
}

// ocrSmokeNoPages returns a 2xx body without a pages list.
func ocrSmokeNoPages(_ context.Context, _, _ string, _ any, _ map[string]string, _ time.Duration, _ bool) (Object, error) {
	return Object{"provider": "mistral"}, nil
}

func setSmoke(t *testing.T, seam func(context.Context, string, string, any, map[string]string, time.Duration, bool) (Object, error)) {
	t.Helper()
	old := profileSmokeRequest
	profileSmokeRequest = seam
	t.Cleanup(func() { profileSmokeRequest = old })
}

func TestVerifyProfileRoutesRunsOnlyOCR(t *testing.T) {
	setSmoke(t, ocrSmokeOK)
	cfg := clientConfig{Version: 1, Gateway: "https://spark.example:8443/"}
	capabilities := []any{
		Object{"name": "chat", "providers": []any{}},
		Object{"name": "ocr", "providers": []any{}},
		Object{"name": "speech", "providers": []any{}},
		Object{"name": "ocr", "providers": []any{}}, // duplicate must not double-run
	}
	out := verifyProfileRoutes(context.Background(), cfg, capabilities)
	if len(out) != 3 {
		t.Fatalf("expected 3 entries, got %v", out)
	}
	if str(out[0]["status"]) != "not_verified" {
		t.Fatalf("chat must not be reported verified: %v", out[0])
	}
	if str(out[1]["status"]) != "verified" {
		t.Fatalf("ocr must be verified: %v", out[1])
	}
	if str(out[2]["status"]) != "not_verified" {
		t.Fatalf("speech must not be reported verified: %v", out[2])
	}
}

func TestVerifyProfileRoutesOCRFailure(t *testing.T) {
	setSmoke(t, ocrSmokeErr)
	cfg := clientConfig{Version: 1, Gateway: "https://spark.example:8443"}
	out := verifyProfileRoutes(context.Background(), cfg, []any{Object{"name": "ocr"}})
	if len(out) != 1 || str(out[0]["status"]) != "failed" {
		t.Fatalf("transport failure must be reported failed: %v", out)
	}
	if !strings.Contains(str(out[0]["detail"]), "Cannot reach") {
		t.Fatalf("failure detail lost: %v", out[0])
	}
}

func TestVerifyProfileRoutesOCRMalformedResponse(t *testing.T) {
	setSmoke(t, ocrSmokeNoPages)
	cfg := clientConfig{Version: 1, Gateway: "https://spark.example:8443"}
	out := verifyProfileRoutes(context.Background(), cfg, []any{Object{"name": "ocr"}})
	if len(out) != 1 || str(out[0]["status"]) != "failed" {
		t.Fatalf("a 2xx without pages must not be reported verified: %v", out)
	}
}

func TestOCRSmokeSendsTinyRequest(t *testing.T) {
	var gotMethod string
	var gotURL string
	var gotPayload Object
	setSmoke(t, func(_ context.Context, method, url string, payload any, _ map[string]string, _ time.Duration, _ bool) (Object, error) {
		gotMethod, gotURL = method, url
		if o, ok := payload.(Object); ok {
			gotPayload = o
		}
		return Object{"pages": []any{Object{}}}, nil
	})
	cfg := clientConfig{Version: 1, Gateway: "https://spark.example:8443/"}
	verifyProfileRoutes(context.Background(), cfg, []any{Object{"name": "ocr"}})
	if gotMethod != http.MethodPost {
		t.Fatalf("expected POST, got %s", gotMethod)
	}
	if gotURL != "https://spark.example:8443/v1/ocr" {
		t.Fatalf("unexpected route: %s", gotURL)
	}
	if str(gotPayload["model"]) != profileSmokeModel {
		t.Fatalf("unexpected model: %v", gotPayload["model"])
	}
	doc := obj(gotPayload["document"])
	if str(doc["type"]) != "image_url" {
		t.Fatalf("unexpected document type: %v", doc["type"])
	}
	url := str(doc["image_url"])
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("expected a data URI png: %q", url)
	}
	if len(url) < 32 {
		t.Fatalf("tiny image missing: %q", url)
	}
}

func TestFoldProfileVerificationSkipsNonApplied(t *testing.T) {
	setSmoke(t, ocrSmokeOK)
	cfg := clientConfig{Version: 1, Gateway: "https://spark.example:8443"}
	planned := Object{"status": "planned", "capabilities": []any{Object{"name": "ocr"}}}
	foldProfileVerification(context.Background(), cfg, planned)
	if _, ok := planned["route_verification"]; ok {
		t.Fatal("plans must not run verification")
	}
	empty := Object{"status": "applied", "capabilities": []any{}}
	foldProfileVerification(context.Background(), cfg, empty)
	if _, ok := empty["route_verification"]; ok {
		t.Fatal("empty capabilities must not run verification")
	}
	applied := Object{"status": "applied", "capabilities": []any{Object{"name": "ocr"}}}
	foldProfileVerification(context.Background(), cfg, applied)
	if _, ok := applied["route_verification"]; !ok {
		t.Fatal("applied profile must run verification")
	}
	if profileVerificationFailed(applied) {
		t.Fatal("verified checks must not fail")
	}
	setSmoke(t, ocrSmokeErr)
	applied["route_verification"] = nil
	foldProfileVerification(context.Background(), cfg, applied)
	if !profileVerificationFailed(applied) {
		t.Fatal("failed OCR check must fail the command")
	}
}

func TestRouteVerificationBlockRenders(t *testing.T) {
	result := Object{"route_verification": []any{
		Object{"capability": "ocr", "status": "verified", "detail": "reached the service"},
		Object{"capability": "speech", "status": "not_verified", "detail": "no probe"},
	}}
	out := routeVerificationBlock(result)
	for _, want := range []string{"Route verification", "OCR: verified", "Speech: not verified", "reached the service"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if routeVerificationBlock(Object{}) != "" {
		t.Error("empty route verification must not render")
	}
}

func TestRouteVerificationFailureRendersFailed(t *testing.T) {
	result := Object{"route_verification": []any{
		Object{"capability": "ocr", "status": "failed", "detail": "Server returned HTTP 502."},
	}}
	if !strings.Contains(routeVerificationBlock(result), "OCR: FAILED") {
		t.Fatalf("failed check did not render prominently:\n%s", routeVerificationBlock(result))
	}
}

func TestOCRStatusUsesPayloadHost(t *testing.T) {
	root := t.TempDir()
	source := ocrInstalled(t, root)
	service, err := newOCRServiceOnHost(root, "spark02")
	if err != nil {
		t.Fatal(err)
	}
	// Same minimal Docker behavior as the other status tests.
	ocrSetStub(t, func(args []string) (CommandResult, error) {
		return CommandResult{Code: 0, Out: ocrInspectOK}, nil
	})
	ocrSetHealth(t, Object{"service": "spark-ocr"}, nil)
	ocrWriteRecord(t, service, Object{"source": source, "revision": "abc123", "project": "deploy"})
	// Local spark01-style payload for the spark01 host.
	local, err := ManageOCR(root, Object{"action": "status"})
	if err != nil {
		t.Fatal(err)
	}
	if str(local["host"]) != "spark01" {
		t.Fatalf("default host must be spark01: %v", local["host"])
	}
	// Remote payloads carry the actual node so reports name the right Spark.
	remote, err := ManageOCR(root, Object{"action": "status", "host": "spark02"})
	if err != nil {
		t.Fatal(err)
	}
	if str(remote["host"]) != "spark02" {
		t.Fatalf("remote host must be spark02: %v", remote["host"])
	}
	if len(remote) != 10 {
		t.Fatalf("status must expose exactly the public fields: %v", remote)
	}
}
