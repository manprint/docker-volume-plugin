package dvol

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ComposeVolume is a named volume declared in a compose file.
type ComposeVolume struct {
	// Key is the name used under the top-level `volumes:` key.
	Key string `json:"key"`
	// Name is the actual docker volume name the project will use.
	Name string `json:"name"`
}

// ComposeFile is the parsed subset of a compose file needed for backup.
type ComposeFile struct {
	// Project is the compose project name (from `name:`, --project-name or the
	// directory of the compose file).
	Project string `json:"project"`
	// Volumes is the list of named volumes declared in the file.
	Volumes []ComposeVolume `json:"volumes"`
}

// LoadComposeFile parses a compose file and resolves the project name and its
// named volumes. projectOverride comes from -p/--project-name ("" if unset).
func LoadComposeFile(path, projectOverride string) (*ComposeFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}

	cf := &ComposeFile{}
	project, source := "", ""
	if n, ok := root["name"].(string); ok && n != "" {
		project, source = n, "top-level name"
	}
	if envProject := os.Getenv("COMPOSE_PROJECT_NAME"); envProject != "" {
		project, source = envProject, "COMPOSE_PROJECT_NAME"
	}
	if projectOverride != "" {
		project, source = projectOverride, "-p/--project-name"
	}
	if project == "" {
		project = defaultProject(path)
		source = "compose file directory"
	}
	if err := validateProject(project); err != nil {
		return nil, fmt.Errorf("invalid Compose project from %s: %w", source, err)
	}
	cf.Project = project

	raw, _ := root["volumes"].(map[string]any)
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m, _ := raw[k].(map[string]any)
		cf.Volumes = append(cf.Volumes, ComposeVolume{
			Key:  k,
			Name: composeVolumeName(cf.Project, k, m),
		})
	}
	return cf, nil
}

// defaultProject derives a valid project name from the Compose file directory.
// Explicit names are validated rather than silently rewritten.
func defaultProject(path string) string {
	dir := filepath.Dir(path)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return normalizeDefaultProject(filepath.Base(dir))
}

var (
	projectNameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	projectInvalidRe = regexp.MustCompile(`[^a-z0-9_-]+`)
)

func validateProject(p string) error {
	if !projectNameRe.MatchString(p) {
		return fmt.Errorf("project name %q must start with a lowercase letter or digit and contain only lowercase letters, digits, dashes and underscores", p)
	}
	return nil
}

func normalizeDefaultProject(p string) string {
	p = strings.ToLower(p)
	p = projectInvalidRe.ReplaceAllString(p, "")
	p = strings.TrimLeft(p, "-_")
	if p == "" {
		p = "default"
	}
	return p
}

// composeVolumeName resolves the actual docker volume name for a compose
// volume key: `name:` override wins, then external volumes use their own
// name, otherwise the volume is created as "<project>_<key>".
func composeVolumeName(project, key string, m map[string]any) string {
	if m == nil {
		return projectName(project, key)
	}
	if n, ok := m["name"].(string); ok && n != "" {
		return n
	}
	switch ext := m["external"].(type) {
	case bool:
		if ext {
			return key
		}
	case string:
		if ext != "" {
			return ext
		}
	case map[string]any:
		if n, ok := ext["name"].(string); ok && n != "" {
			return n
		}
	}
	return projectName(project, key)
}

func projectName(project, key string) string {
	if project == "" {
		return key
	}
	return project + "_" + key
}
