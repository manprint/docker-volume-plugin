package dvol

import (
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
	if n, ok := root["name"].(string); ok && n != "" {
		cf.Project = normalizeProject(n)
	}
	if projectOverride != "" {
		cf.Project = normalizeProject(projectOverride)
	}
	if cf.Project == "" {
		cf.Project = defaultProject(path)
	}

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

// defaultProject derives the project name from the compose file directory,
// mirroring docker compose: the lowercased basename with invalid characters
// replaced by "-".
func defaultProject(path string) string {
	dir := filepath.Dir(path)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return normalizeProject(filepath.Base(dir))
}

// projectCharRe matches characters docker compose strips from project names.
var projectCharRe = regexp.MustCompile(`[^a-z0-9_.-]+`)

// normalizeProject lowercases the name and replaces invalid characters with
// "-", mirroring docker compose's own normalization of `name:` and -p values
// so derived volume names match the ones docker actually creates.
func normalizeProject(p string) string {
	p = strings.ToLower(p)
	p = projectCharRe.ReplaceAllString(p, "-")
	// docker compose requires a project name to start with a letter or a digit.
	// Keeping a leading "." or "-" here would also produce a hidden bundle file
	// named after the project.
	p = strings.TrimLeft(p, ".-_")
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
