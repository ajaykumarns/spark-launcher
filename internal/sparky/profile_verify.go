package sparky

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"time"
)

// profileSmokeRequest is the injectable seam for the gateway smoke call;
// tests never open real network connections.
var profileSmokeRequest = requestObject

// profileSmokeTimeout bounds one route verification so an unreachable
// gateway is reported as failed, not as a hang.
const profileSmokeTimeout = 90 * time.Second

// profileSmokeModel is the OCR model the verification exercises: the same
// English model the daily digest uses through the gateway.
const profileSmokeModel = "mistral/paddleocr-english"

// profileSmokeImage is a tiny valid PNG with text-like contrast. It is
// deterministic: OCR's answer for it only needs to reach the route, so the
// smoke can never be mistaken for a real document.
func profileSmokeImage() []byte {
	const width, height = 96, 48
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	black := color.NRGBA{R: 0, G: 0, B: 0, A: 255}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, white)
		}
	}
	for y := 9; y <= 24; y++ {
		for x := 8; x <= 88; x++ {
			img.Set(x, y, black)
		}
	}
	for y := 30; y <= 40; y++ {
		for x := 8; x <= 64; x++ {
			img.Set(x, y, black)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}

// profileOCRSmoke sends one tiny OCR request through the gateway. A 2xx
// with a pages list means the route reaches the OCR service on its configured
// node; anything else — transport failure, 5xx, malformed body — is reported
// failed with the detail.
func profileOCRSmoke(ctx context.Context, cfg clientConfig) Object {
	payload := Object{
		"model": profileSmokeModel,
		"document": Object{
			"type":     "image_url",
			"image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(profileSmokeImage()),
		},
	}
	origin := strings.TrimSuffix(cfg.Gateway, "/")
	out, err := profileSmokeRequest(ctx, http.MethodPost, origin+"/v1/ocr", payload, nil, profileSmokeTimeout, false)
	if err != nil {
		return Object{"capability": "ocr", "status": "failed", "detail": err.Error()}
	}
	if arr(out["pages"]) == nil {
		return Object{"capability": "ocr", "status": "failed", "detail": "the OCR route answered without pages"}
	}
	return Object{"capability": "ocr", "status": "verified", "detail": "the OCR route reached the OCR service through the gateway"}
}

// verifyProfileRoutes runs one route check per capability in a profile. A
// capability is only reported verified when a real request succeeds; anything
// else is reported not_verified so the report cannot be mistaken for proof.
func verifyProfileRoutes(ctx context.Context, cfg clientConfig, capabilities []any) []Object {
	seen := map[string]bool{}
	out := []Object{}
	for _, raw := range capabilities {
		group := obj(raw)
		name := str(group["name"])
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		switch name {
		case "ocr":
			out = append(out, profileOCRSmoke(ctx, cfg))
		default:
			out = append(out, Object{"capability": name, "status": "not_verified",
				"detail": "no verifiable route probe exists yet for this capability"})
		}
	}
	return out
}

// foldProfileVerification runs the route checks for the capabilities of an
// applied profile and stores them on data["route_verification"]. It is
// silent about plan/dry-run results: only data marked applied carries routes.
func foldProfileVerification(ctx context.Context, cfg clientConfig, data Object) {
	if str(data["status"]) != "applied" {
		return
	}
	capabilities := arr(data["capabilities"])
	if len(capabilities) == 0 {
		return
	}
	data["route_verification"] = verifyProfileRoutes(ctx, cfg, capabilities)
}

// profileVerificationFailed reports whether any route verification failed,
// so the caller can decide how to report it.
func profileVerificationFailed(data Object) bool {
	for _, raw := range arr(data["route_verification"]) {
		if str(obj(raw)["status"]) == "failed" {
			return true
		}
	}
	return false
}

func routeStatusLabel(status string) string {
	switch status {
	case "verified":
		return "verified"
	case "failed":
		return "FAILED"
	default:
		return "not verified"
	}
}

// routeVerificationBlock renders the "Route verification" section of a
// profile report. It returns nothing when there is nothing to report.
func routeVerificationBlock(result Object) string {
	verification := arr(result["route_verification"])
	if len(verification) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString("Route verification\n")
	for _, raw := range verification {
		check := obj(raw)
		fmt.Fprintf(&out, "  %s: %s", profileLabel(str(check["capability"])), routeStatusLabel(str(check["status"])))
		if detail := strings.Join(strings.Fields(str(check["detail"])), " "); detail != "" {
			fmt.Fprintf(&out, " — %s", detail)
		}
		out.WriteString("\n")
	}
	return out.String()
}
