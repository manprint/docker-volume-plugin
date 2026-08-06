package dvol

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadComposeFile(t *testing.T) {
	p := writeTemp(t, `
name: myproj
volumes:
  data:
  cache:
    name: cache-data
  ext:
    external: true
  extnamed:
    external:
      name: shared-ext
`)
	cf, err := LoadComposeFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if cf.Project != "myproj" {
		t.Fatalf("project=%q", cf.Project)
	}
	got := map[string]string{}
	for _, v := range cf.Volumes {
		got[v.Key] = v.Name
	}
	want := map[string]string{
		"data":     "myproj_data",
		"cache":    "cache-data",
		"ext":      "ext",
		"extnamed": "shared-ext",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: got %q want %q", k, got[k], w)
		}
	}
}

func TestLoadComposeFileOverrideProject(t *testing.T) {
	p := writeTemp(t, "volumes:\n  data:\n")
	cf, err := LoadComposeFile(p, "other")
	if err != nil {
		t.Fatal(err)
	}
	if cf.Project != "other" {
		t.Fatalf("project=%q", cf.Project)
	}
	if len(cf.Volumes) != 1 || cf.Volumes[0].Name != "other_data" {
		t.Fatalf("volumes=%+v", cf.Volumes)
	}
}

func TestDefaultProjectFromDir(t *testing.T) {
	p := writeTemp(t, "volumes:\n  data:\n")
	cf, err := LoadComposeFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if cf.Project == "" {
		t.Fatal("expected default project from dir")
	}
}
