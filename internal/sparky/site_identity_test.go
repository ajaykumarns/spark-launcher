package sparky

import (
	"os"
	"path/filepath"
	"testing"
)

// Optional deployment audit. It reads copied site files and changes no service.
func TestSiteModelIdentityAcrossEveryProfile(t *testing.T) {
	dir := os.Getenv("SPARKY_SITE_DIR")
	if dir == "" {
		t.Skip("set SPARKY_SITE_DIR to copied site configuration")
	}
	cfg, err := readCluster(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	file, err := loadProfileFile(filepath.Join(dir, "profiles.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for name := range file.Profiles {
		t.Run(name, func(t *testing.T) {
			p, err := file.profile(name, cfg)
			if err != nil {
				t.Fatal(err)
			}
			selected := copyObject(cfg)
			models := copyObject(obj(cfg["models"]))
			selected["models"] = models
			for _, m := range p.models {
				row := copyObject(obj(models[m.name]))
				row["run_on"] = m.placement
				models[m.name] = row
			}
			catalog, err := clientMakeCatalog(harnessTestConfig(t, nil), decodeResponse(t, string(jsonBytes(modelCatalog(selected)))))
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range p.models {
				if str(m.model["api_kind"]) == "decision" {
					continue
				}
				found, err := clientResolve(catalog, m.name)
				if err != nil || str(found["id"]) != "garuda/"+publicModel(cfg, m.name) {
					t.Fatalf("%s: %v %v", m.name, found, err)
				}
			}
		})
	}
}
