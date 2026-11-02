package sparky

import (
	"encoding/json"
	"strings"
	"testing"
)

func planSnapshot() Object {
	return Object{
		"models": Object{
			"busy": Object{"kind": "llm", "loaded": true, "nodes": []any{"spark01"}, "active_requests": 2},
			"idle": Object{"kind": "llm", "loaded": true, "nodes": []any{"spark02"}, "active_requests": 0},
		},
		"comfyui": Object{"spark01": Object{"executing": false, "running": 0}, "spark02": Object{"executing": false, "running": 0}},
	}
}

func TestMakePlanEvictsOnlyActualConflicts(t *testing.T) {
	c := baseConfig()
	c["models"] = Object{"target": llmModel("spark01"), "busy": llmModel("both"), "idle": llmModel("spark02")}
	plan, err := makePlan(c, "target", planSnapshot(), "load", "")
	if err != nil {
		t.Fatalf("plan failed: %v", err)
	}
	// Only the model actually loaded on a requested device is evicted.
	if !contains(stringsOf(plan["wait_for"]), "busy") {
		t.Fatalf("the busy conflicting model must be waited on: %#v", plan["wait_for"])
	}
	if contains(stringsOf(plan["wait_for"]), "idle") {
		t.Fatal("the idle non-conflicting model must not be waited on")
	}
	unloaded := []string{}
	for _, s := range arr(plan["steps"]) {
		if obj(s)["action"] == "unload" {
			unloaded = append(unloaded, str(obj(s)["workload"]))
		}
	}
	if len(unloaded) != 1 || unloaded[0] != "busy" {
		t.Fatalf("exactly the conflicting model must be evicted, got %v", unloaded)
	}
}

func TestMakePlanNeverEvictsProtectedModels(t *testing.T) {
	c := baseConfig()
	protected := llmModel("both")
	protected["keep_loaded"] = true
	c["models"] = Object{"target": llmModel("both"), "busy": protected}
	_, err := makePlan(c, "target", planSnapshot(), "load", "")
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("protected model must not be evicted: %v", err)
	}
}

// TestMakePlanHonoursProtectedModelsPolicy proves the plan preview refuses
// to evict a model protected through policy.protected_models even when its
// snapshot row lacks keep_loaded (the snapshot only carries loaded state).
func TestMakePlanHonoursProtectedModelsPolicy(t *testing.T) {
	c := baseConfig()
	c["policy"] = Object{"protected_models": []any{"busy"}}
	c["models"] = Object{"target": llmModel("both"), "busy": llmModel("spark01")}
	_, err := makePlan(c, "target", planSnapshot(), "load", "")
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("policy-protected model must not be evicted: %v", err)
	}
}

func TestMakePlanKeepRunningAndPlacementMoves(t *testing.T) {
	c := baseConfig()
	c["models"] = Object{"target": llmModel("spark01")}
	snapshot := Object{
		"models":  Object{"target": Object{"kind": "llm", "loaded": true, "nodes": []any{"spark01"}, "active_requests": 0}},
		"comfyui": Object{"spark01": Object{"executing": false, "running": 0}},
	}
	plan, err := makePlan(c, "target", snapshot, "load", "")
	if err != nil {
		t.Fatalf("plan failed: %v", err)
	}
	if len(arr(plan["steps"])) != 1 || obj(arr(plan["steps"])[0])["action"] != "keep_running" {
		t.Fatalf("an already correctly placed model must keep running: %#v", plan["steps"])
	}
	if len(stringsOf(plan["wait_for"])) != 0 {
		t.Fatalf("keep_running must wait on nothing: %#v", plan["wait_for"])
	}

	moving := Object{
		"models":  Object{"target": Object{"kind": "llm", "loaded": true, "nodes": []any{"spark02"}, "active_requests": 3}},
		"comfyui": Object{"spark01": Object{"executing": false, "running": 0}},
	}
	plan, err = makePlan(c, "target", moving, "load", "")
	if err != nil {
		t.Fatalf("placement move plan failed: %v", err)
	}
	found := false
	for _, s := range arr(plan["steps"]) {
		if obj(s)["action"] == "restart_for_placement" {
			found = true
			if !sameStrings(stringsOf(obj(s)["from"]), []string{"spark02"}) || !sameStrings(stringsOf(obj(s)["to"]), []string{"spark01"}) {
				t.Fatalf("restart must record the actual placement move: %#v", s)
			}
		}
	}
	if !found {
		t.Fatal("a placement move must be planned as a restart")
	}
	if !contains(stringsOf(plan["wait_for"]), "target") {
		t.Fatal("an active placement move must wait for the model's own jobs")
	}
}

func TestMakePlanRejectsInvalidRequests(t *testing.T) {
	c := baseConfig()
	c["models"] = Object{"tts": audioModel("spark02"), "sway": Object{"kind": "comfyui", "run_on": "both", "settings": Object{"both": Object{"parameters": Object{}}}}}
	if _, err := makePlan(c, "tts", planSnapshot(), "explode", ""); err == nil {
		t.Fatal("unknown action must fail")
	}
	if _, err := makePlan(c, "tts", planSnapshot(), "unload", ""); err == nil || !strings.Contains(err.Error(), "unload adapter") {
		t.Fatalf("audio unload must fail safely: %v", err)
	}
	plan, err := makePlan(c, "sway", planSnapshot(), "load", "")
	if err != nil {
		t.Fatalf("comfyui plan failed: %v", err)
	}
	if obj(arr(plan["steps"])[0])["action"] != "ensure_comfyui" {
		t.Fatalf("comfy models plan to ensure ComfyUI: %#v", plan["steps"])
	}
	if _, err := makePlan(c, "absent", planSnapshot(), "load", ""); err == nil {
		t.Fatal("unknown workload must fail")
	}
}

func TestMatrixSetsFromActualPlacements(t *testing.T) {
	c := baseConfig()
	c["models"] = Object{
		"shared": llmModel("both"),
		"one":    llmModel("spark01"),
		"two":    llmModel("spark02"),
		"gone": func() Object {
			m := llmModel("spark01")
			obj(m["settings"])["spark01"].(Object)["enabled"] = false
			return m
		}(),
	}
	got := matrixSets(c)
	pairs := []string{}
	for k, v := range got {
		if strings.Contains(str(v), " & ") {
			pairs = append(pairs, k+"="+str(v))
		}
	}
	if len(pairs) != 1 || !strings.HasSuffix(pairs[0], "one & two") {
		t.Fatalf("exactly the disjoint placement pair must be compatible: %#v", got)
	}
	for _, v := range got {
		if strings.Contains(str(v), "shared") && strings.Contains(str(v), " & ") {
			t.Fatalf("a model on both devices cannot share a group: %#v", got)
		}
		if strings.Contains(str(v), "gone") {
			t.Fatal("disabled models must not appear in compatibility groups")
		}
	}
	// Ordering of the input map must not change the output content.
	other := baseConfig()
	other["models"] = Object{"two": llmModel("spark02"), "gone": func() Object {
		m := llmModel("spark01")
		obj(m["settings"])["spark01"].(Object)["enabled"] = false
		return m
	}(), "one": llmModel("spark01"), "shared": llmModel("both")}
	first, _ := json.Marshal(matrixSets(c))
	second, _ := json.Marshal(matrixSets(other))
	if string(first) != string(second) {
		t.Fatalf("compatibility groups depend on ordering:\n%s\n%s", first, second)
	}
}

func TestModelCatalogOmitsDisabledAndComputesAutoBudgets(t *testing.T) {
	c := baseConfig()
	echo := llmModel("spark01")
	obj(obj(echo["settings"])["spark01"])["context_tokens"] = json.Number("8192")
	glm := llmModel("both")
	obj(obj(glm["settings"])["both"])["context_tokens"] = json.Number("32768")
	glm["output_tokens"] = json.Number("4096")
	glm["name"] = "GLM"
	glm["aliases"] = []any{"Flash"}
	c["models"] = Object{"glm": glm, "echo": echo, "tts": audioModel("spark02")}
	ttsModel := obj(c["models"])["tts"].(Object)
	ttsModel["tts"] = Object{"default_voice": "alloy", "api_key": "super-secret"}
	c["auto"] = Object{"targets": []any{"glm", "echo"}, "default": "glm"}
	cat := modelCatalog(c)
	rows := arr(cat["data"])
	if len(rows) != 3 {
		t.Fatalf("catalog must list enabled llms plus spark-auto, got %#v", rows)
	}
	var auto Object
	for _, r := range rows {
		if str(obj(r)["id"]) == "garuda/spark-auto" {
			auto = obj(r)
		}
		if str(obj(r)["id"]) == "garuda/tts" {
			t.Fatal("audio services must stay out of the language model list")
		}
	}
	if auto == nil {
		t.Fatal("spark-auto must be present")
	}
	if n := num(auto["context_length"]); n != 8192 || !integer(n) {
		t.Fatalf("spark-auto context budget wrong: %#v", auto["context_length"])
	}
	if n := num(auto["output_tokens"]); n != 4096 || !integer(n) {
		t.Fatalf("spark-auto output budget wrong: %#v", auto["output_tokens"])
	}
	audio := arr(cat["audio_services"])
	if len(audio) != 1 {
		t.Fatalf("tts must appear once in audio_services: %#v", audio)
	}
	if _, leaked := obj(audio[0])["api_key"]; leaked {
		t.Fatal("non-whitelisted tts metadata must never be public")
	}
	if obj(audio[0])["default_voice"] != "alloy" {
		t.Fatalf("whitelisted tts metadata must be preserved: %#v", audio[0])
	}
	if num(cat["config_version"]) != 1 {
		t.Fatalf("config version must be carried through: %#v", cat["config_version"])
	}
}

func TestSelectPlacementHonorsEnabledAndReason(t *testing.T) {
	c := baseConfig()
	m := llmModel("spark01")
	obj(m["settings"])["spark01"].(Object)["enabled"] = false
	obj(m["settings"])["spark01"].(Object)["reason"] = "not installed on this device"
	c["models"] = Object{"glm": m}
	if _, _, _, _, err := selectPlacement(c, "glm", ""); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("disabled placement must explain itself: %v", err)
	}
	if _, _, _, _, err := selectPlacement(c, "glm", "spark02"); err == nil {
		t.Fatal("missing placement must fail")
	}
}
