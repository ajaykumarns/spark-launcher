package sparky

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// clientConfig holds validated client configuration for the native Go port.
// Field values mirror src/spark_launcher/catalog.py validate_config().
type clientConfig struct {
	Version        int               `json:"version"`
	Gateway        string            `json:"gateway"`
	ModelPrefix    string            `json:"model_prefix"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	CompactPercent int               `json:"compact_percent"`
	DefaultModel   string            `json:"default_model"`
	Binaries       map[string]string `json:"binaries,omitempty"`
	// Extra preserves all raw client fields (admin_ssh_host, worker_ssh_host,
	// remote_root, admin_url, and unknown user settings) so the full public CLI
	// can implement SSH, OCR, devices and UI without rereading another config.
	// Credentials are retained here only and never emitted in human output.
	Extra Object `json:"-"`
}

// MarshalJSON merges the preserved extra fields with the typed base fields.
// Base fields win, so defaults applied during validation are what persist.
func (c clientConfig) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(struct {
		Version        int               `json:"version"`
		Gateway        string            `json:"gateway"`
		ModelPrefix    string            `json:"model_prefix"`
		TimeoutSeconds int               `json:"timeout_seconds"`
		CompactPercent int               `json:"compact_percent"`
		DefaultModel   string            `json:"default_model"`
		Binaries       map[string]string `json:"binaries,omitempty"`
	}{
		Version:        c.Version,
		Gateway:        c.Gateway,
		ModelPrefix:    c.ModelPrefix,
		TimeoutSeconds: c.TimeoutSeconds,
		CompactPercent: c.CompactPercent,
		DefaultModel:   c.DefaultModel,
		Binaries:       c.Binaries,
	})
	if err != nil {
		return nil, err
	}
	base := Object{}
	_ = json.Unmarshal(b, &base)
	return json.Marshal(merge(c.Extra, base))
}

const (
	clientMaxBody = 8 * 1024 * 1024
)

func clientDefaultConfig() clientConfig {
	return clientConfig{
		Version:        1,
		ModelPrefix:    "garuda/",
		TimeoutSeconds: 7200,
		CompactPercent: 95,
	}
}

// clientValidateConfig validates and applies defaults to a raw config object.
func clientValidateConfig(raw Object) (clientConfig, error) {
	cfg := clientDefaultConfig()
	if raw == nil {
		return cfg, fail("Unsupported configuration version.")
	}
	if !integer(raw["version"]) || int(num(raw["version"])) != 1 {
		return cfg, fail("Unsupported configuration version.")
	}
	gateway, ok := raw["gateway"].(string)
	if !ok {
		return cfg, fail("Set gateway to an HTTPS URL in the client configuration.")
	}
	if err := clientValidateGateway(gateway); err != nil {
		return cfg, err
	}
	cfg.Gateway = strings.TrimRight(gateway, "/")
	cfg.ModelPrefix = "garuda/"
	if v, ok := raw["model_prefix"]; ok {
		s, ok := v.(string)
		if !ok || s == "" {
			return cfg, fail("model_prefix must be a nonempty string.")
		}
		cfg.ModelPrefix = s
	}
	if v, ok := raw["compact_percent"]; ok {
		if !integer(v) {
			return cfg, fail("compact_percent must be 50..95.")
		}
		n := int(num(v))
		if n < 50 || n > 95 {
			return cfg, fail("compact_percent must be 50..95.")
		}
		cfg.CompactPercent = n
	}
	if v, ok := raw["timeout_seconds"]; ok {
		if !integer(v) {
			return cfg, fail("timeout_seconds must be 1..86400.")
		}
		n := int(num(v))
		if n < 1 || n > 86400 {
			return cfg, fail("timeout_seconds must be 1..86400.")
		}
		cfg.TimeoutSeconds = n
	}
	if v, ok := raw["default_model"]; ok {
		s, ok := v.(string)
		if !ok {
			return cfg, fail("default_model must be a model name or alias.")
		}
		cfg.DefaultModel = s
	} else {
		cfg.DefaultModel = cfg.ModelPrefix + "spark-auto"
	}
	if _, ok := raw["models"]; ok {
		return cfg, fail("Model settings belong on Spark 1. Remove the unused client models field.")
	}
	cfg.Binaries = map[string]string{}
	if v, ok := raw["binaries"]; ok {
		m, ok := v.(map[string]any)
		if !ok {
			return cfg, fail("Client binaries must be a mapping. Check %s.", clientPath())
		}
		for k, entry := range m {
			s, ok := entry.(string)
			if !ok {
				return cfg, fail("Each binaries entry must be an executable path.")
			}
			cfg.Binaries[k] = s
		}
	}
	cfg.Extra = copyObject(raw)
	return cfg, nil
}

// clientLoadConfig reads and validates the client configuration file.
func clientLoadConfig() (clientConfig, error) {
	raw, err := readObject(clientPath())
	if err != nil {
		if os.IsNotExist(err) {
			return clientConfig{}, fail("Client configuration is missing. Run sparky init --gateway https://your-spark:8443 first.")
		}
		return clientConfig{}, fail("Invalid JSON in %s. Fix the client file.", clientPath())
	}
	return clientValidateConfig(raw)
}

// clientStateCatalogPath is the cached catalog location.
func clientStateCatalogPath() string {
	return filepath.Join(stateDir(), "catalog.json")
}

// clientCatalogPath returns the path used for catalog caching, mirroring the
// Python state_dir()/catalog.json layout.
func clientCatalogPath() string { return clientStateCatalogPath() }

// clientValidateGateway checks that the gateway is a bare origin.
func clientValidateGateway(gateway string) error {
	u, err := parseOriginURL(gateway)
	if err != nil {
		return fail("Set gateway to an HTTPS URL in the client configuration.")
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return fail("Gateway must be an origin, without credentials, /v1, or a query.")
	}
	if u.Scheme != "https" {
		host := u.Hostname()
		if !(u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")) {
			return fail("Use HTTPS for a remote gateway.")
		}
	}
	return nil
}

// clientMakeCatalog normalizes a raw wire catalog into the public JSON shape.
func clientMakeCatalog(cfg clientConfig, wire Object) (Object, error) {
	data, ok := wire["data"].([]any)
	if !ok {
		return nil, fail("The gateway returned an invalid model catalog.")
	}
	prefix := cfg.ModelPrefix
	models := []Object{}
	var auto Object
	seen := map[string]bool{}
	for _, rowAny := range data {
		row, ok := rowAny.(map[string]any)
		if !ok {
			return nil, fail("The gateway returned an invalid model entry.")
		}
		modelID, ok := row["id"].(string)
		if !ok {
			return nil, fail("The gateway returned an invalid model entry.")
		}
		if !strings.HasPrefix(modelID, prefix) {
			continue
		}
		if seen[modelID] {
			return nil, fail("The gateway returned duplicate model IDs.")
		}
		seen[modelID] = true
		for _, field := range []string{"aliases", "inputs", "nodes"} {
			if v, present := row[field]; present {
				if _, ok := v.([]any); !ok {
					return nil, fail("The gateway returned invalid %s metadata.", field)
				}
				for _, item := range arr(v) {
					s, ok := item.(string)
					if !ok || strings.TrimSpace(s) == "" {
						return nil, fail("The gateway returned invalid %s metadata.", field)
					}
				}
			}
		}
		if v, present := row["output_tokens"]; present {
			if !integer(v) || int(num(v)) <= 0 {
				return nil, fail("The gateway returned an invalid output token budget.")
			}
		}
		if v, present := row["name"]; present {
			if _, ok := v.(string); !ok {
				return nil, fail("The gateway returned an invalid model name.")
			}
		}
		if modelID == prefix+"spark-auto" {
			auto = row
			continue
		}
		if !integer(row["context_length"]) {
			continue
		}
		ctxTokens := int(num(row["context_length"]))
		if ctxTokens <= 0 {
			continue
		}
		model := Object{}
		for _, k := range []string{"name", "aliases", "output_tokens", "inputs", "reasoning", "nodes"} {
			if v, present := row[k]; present {
				model[k] = v
			}
		}
		model["id"] = modelID
		model["context_tokens"] = ctxTokens
		if _, present := model["name"]; !present {
			model["name"] = strings.TrimPrefix(modelID, prefix)
		}
		if ot, present := model["output_tokens"]; present && int(num(ot)) >= ctxTokens {
			return nil, fail("Output budget exceeds the live context for %s.", modelID)
		}
		models = append(models, model)
	}
	if auto != nil && len(models) > 0 && !(auto["dynamic"] == true && auto["available"] == false) {
		minimum := 0
		for i, m := range models {
			if i == 0 || num(m["context_tokens"]) < float64(minimum) {
				minimum = int(num(m["context_tokens"]))
			}
		}
		if integer(auto["context_length"]) && int(num(auto["context_length"])) > 0 {
			minimum = int(num(auto["context_length"]))
		}
		autoModel := Object{}
		for _, k := range []string{"name", "aliases", "output_tokens", "inputs", "routing", "request_model"} {
			if v, present := auto[k]; present {
				autoModel[k] = v
			}
		}
		autoModel["id"] = auto["id"]
		autoModel["context_tokens"] = minimum
		autoModel["context_source"] = "gateway auto route context"
		if _, present := autoModel["name"]; !present {
			autoModel["name"] = "Spark Auto"
		}
		if ot, present := autoModel["output_tokens"]; present && int(num(ot)) >= minimum {
			return nil, fail("Auto output budget exceeds the shared context.")
		}
		models = append(models, autoModel)
	}
	if len(models) == 0 {
		return nil, fail("No language models with usable context metadata were found.")
	}
	audio, err := clientValidateAudio(wire)
	if err != nil {
		return nil, err
	}
	sort.Slice(models, func(i, j int) bool { return str(models[i]["id"]) < str(models[j]["id"]) })
	normalized := make([]any, 0, len(models))
	for _, m := range models {
		normalized = append(normalized, m)
	}
	return Object{
		"gateway":        cfg.Gateway,
		"fetched_at":     now(),
		"audio_services": audio,
		"models":         normalized,
	}, nil
}

// clientValidateAudio validates and returns speech service metadata.
func clientValidateAudio(wire Object) ([]any, error) {
	audio, present := wire["audio_services"]
	if !present {
		return []any{}, nil
	}
	list, ok := audio.([]any)
	if !ok {
		return nil, fail("The gateway returned invalid speech service metadata.")
	}
	for _, item := range list {
		s, ok := item.(map[string]any)
		if !ok {
			return nil, fail("The gateway returned invalid speech service metadata.")
		}
		if str(s["kind"]) != "tts" {
			return nil, fail("The gateway returned invalid speech service metadata.")
		}
		for _, k := range []string{"id", "model", "api_base", "default_voice"} {
			if _, ok := s[k].(string); !ok {
				return nil, fail("The gateway returned invalid speech service metadata.")
			}
		}
		voices, ok := s["voices"].([]any)
		if !ok {
			return nil, fail("The gateway returned invalid speech service metadata.")
		}
		for _, v := range voices {
			if _, ok := v.(string); !ok {
				return nil, fail("The gateway returned invalid speech service metadata.")
			}
		}
	}
	return list, nil
}

// clientFetchCatalog returns the live or cached catalog.
func clientFetchCatalog(cfg clientConfig, offline bool) (Object, error) {
	path := clientCatalogPath()
	if offline {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fail("No usable cached model list. Connect to the gateway and run sparky models first.")
		}
		catalog, derr := decode(raw)
		if derr != nil {
			return nil, fail("No usable cached model list. Connect to the gateway and run sparky models first.")
		}
		if _, ok := catalog["models"].([]any); !ok {
			return nil, fail("No usable cached model list. Connect to the gateway and run sparky models first.")
		}
		if _, ok := catalog["fetched_at"].(string); !ok {
			return nil, fail("No usable cached model list. Connect to the gateway and run sparky models first.")
		}
		// Re-apply live metadata checks to cached entries.
		replay := Object{"data": []any{}}
		for _, mAny := range arr(catalog["models"]) {
			m, ok := mAny.(map[string]any)
			if !ok {
				return nil, fail("No usable cached model list. Connect to the gateway and run sparky models first.")
			}
			row := copyObject(m)
			if _, present := row["context_length"]; !present {
				row["context_length"] = row["context_tokens"]
			}
			replay["data"] = append(arr(replay["data"]), row)
		}
		if _, verr := clientMakeCatalog(cfg, replay); verr != nil {
			return nil, fail("No usable cached model list. Connect to the gateway and run sparky models first.")
		}
		if str(catalog["gateway"]) != cfg.Gateway {
			return nil, fail("Cached catalog belongs to another gateway.")
		}
		return catalog, nil
	}
	wire, err := requestObject(context.Background(), "GET", cfg.Gateway+"/sparky/catalog", nil, nil, 20*1000*1000*1000, false)
	if err != nil {
		return nil, err
	}
	catalog, err := clientMakeCatalog(cfg, wire)
	if err != nil {
		return nil, err
	}
	if err := writeObject(path, catalog); err != nil {
		return nil, err
	}
	return catalog, nil
}

// clientResolve resolves a model name or alias against a catalog.
func clientResolve(catalog Object, name string) (Object, error) {
	requested := strings.ToLower(strings.TrimSpace(name))
	type entry struct {
		alias string
		model Object
	}
	aliases := map[string][]Object{}
	order := []string{}
	for _, mAny := range arr(catalog["models"]) {
		m, ok := mAny.(map[string]any)
		if !ok {
			continue
		}
		modelID := str(m["id"])
		names := []string{modelID}
		if _, suffix, found := strings.Cut(modelID, "/"); found {
			names = append(names, suffix)
		}
		names = append(names, stringsOf(m["aliases"])...)
		seenAlias := map[string]bool{}
		for _, n := range names {
			key := strings.ToLower(n)
			if seenAlias[key] {
				continue
			}
			seenAlias[key] = true
			if _, exists := aliases[key]; !exists {
				order = append(order, key)
			}
			aliases[key] = append(aliases[key], m)
		}
	}
	matches := aliases[requested]
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, str(m["id"]))
		}
		return nil, fail("Ambiguous model alias: %s. Use one of: %s.", name, strings.Join(ids, ", "))
	}
	closest := clientClosestMatch(requested, order)
	hint := ""
	if closest != "" {
		hint = " Did you mean " + closest + "?"
	}
	return nil, fail("Unknown model: %s.%s Run sparky models to see available names.", name, hint)
}

// parseOriginURL parses a gateway URL string.
func parseOriginURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fail("empty url")
	}
	return url.Parse(raw)
}

// clientClosestMatch finds a near match for an unknown alias (difflib-like).
func clientClosestMatch(requested string, candidates []string) string {
	best, bestScore := "", 0.0
	for _, c := range candidates {
		score := clientSimilarity(requested, c)
		if score > bestScore {
			best, bestScore = c, score
		}
	}
	if bestScore < 0.6 {
		return ""
	}
	return best
}

// clientSimilarity returns a SequenceMatcher-like ratio of two strings.
func clientSimilarity(a, b string) float64 {
	if a == b {
		return 1
	}
	la, lb := len([]rune(a)), len([]rune(b))
	if la == 0 || lb == 0 {
		return 0
	}
	// Longest common subsequence-based ratio.
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	ra, rb := []rune(a), []rune(b)
	for i := 1; i <= la; i++ {
		for j := 1; j <= lb; j++ {
			if ra[i-1] == rb[j-1] {
				cur[j] = prev[j-1] + 1
			} else if cur[j-1] > prev[j] {
				cur[j] = cur[j-1]
			} else {
				cur[j] = prev[j]
			}
		}
		prev, cur = cur, prev
	}
	lcs := prev[lb]
	return (2 * float64(lcs)) / float64(la+lb)
}
