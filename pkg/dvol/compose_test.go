package dvol

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	t.Setenv("COMPOSE_PROJECT_NAME", "")
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

func TestInvalidTopLevelProjectNameIsRejected(t *testing.T) {
	p := writeTemp(t, "name: My Proj!\nvolumes:\n  data:\n")
	if _, err := LoadComposeFile(p, ""); err == nil {
		t.Fatal("invalid top-level Compose project name was silently normalized")
	}
}

func TestInvalidProjectOverrideIsRejected(t *testing.T) {
	p := writeTemp(t, "volumes:\n  data:\n")
	if _, err := LoadComposeFile(p, "UPPER CASE"); err == nil {
		t.Fatal("invalid -p project name was silently normalized")
	}
	if _, err := LoadComposeFile(p, "has.dot"); err == nil {
		t.Fatal("project name containing a dot was accepted")
	}
}

func TestComposeProjectPrecedenceIncludesEnvironment(t *testing.T) {
	p := writeTemp(t, "name: from-file\nvolumes:\n  data:\n")
	t.Setenv("COMPOSE_PROJECT_NAME", "from-env")
	cf, err := LoadComposeFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if cf.Project != "from-env" || cf.Volumes[0].Name != "from-env_data" {
		t.Fatalf("environment project was not applied: %+v", cf)
	}
	cf, err = LoadComposeFile(p, "from-flag")
	if err != nil {
		t.Fatal(err)
	}
	if cf.Project != "from-flag" || cf.Volumes[0].Name != "from-flag_data" {
		t.Fatalf("-p did not override environment: %+v", cf)
	}
}
