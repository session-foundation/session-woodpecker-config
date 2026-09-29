// Package workflow turns evaluated pipeline configs into named Woodpecker workflow files.
package workflow

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Workflow is a single Woodpecker workflow: one config file, as far as Woodpecker is concerned.
type Workflow struct {
	Name   string
	Config map[string]any
}

// FileName is the config file name Woodpecker will derive the workflow's name from.
func (w Workflow) FileName() string {
	return w.Name + ".yaml"
}

// YAML renders the workflow's config.
func (w Workflow) YAML() (string, error) {
	b, err := yaml.Marshal(w.Config)
	if err != nil {
		return "", fmt.Errorf("workflow %q: %w", w.Name, err)
	}
	return string(b), nil
}

// TrivialLabels are the agent labels for the workflows this service adds that do next to nothing
// (the All builds check and the .drone.jsonnet deprecation notice).  They go to a dedicated agent
// with WOODPECKER_AGENT_LABELS=!trivial=yes, so that they don't wait for a free build slot.
func TrivialLabels() map[string]any {
	return map[string]any{"backend": "docker", "trivial": "yes"}
}

var slash = regexp.MustCompile(`\s*/\s*`)

// SanitizeName makes name safe to use as a workflow name.  Woodpecker takes a workflow's name from
// the base name of its config file, so a "/" in the name would silently truncate it.
func SanitizeName(name string) string {
	return strings.TrimSpace(slash.ReplaceAllString(name, ": "))
}

// NameFromFile returns the workflow name Woodpecker derives from a config file name.
func NameFromFile(file string) string {
	name := path.Base(file)
	name = strings.TrimSuffix(name, ".yml")
	name = strings.TrimSuffix(name, ".yaml")
	return strings.TrimPrefix(name, ".")
}

// FromResult extracts workflows from the value a config evaluated to: either a list of workflow
// objects, each with a "name", or a single workflow object whose name defaults to defaultName.
// The "name" field itself is not part of Woodpecker's workflow syntax, so it is removed.
func FromResult(result any, defaultName string) ([]Workflow, error) {
	switch v := result.(type) {
	case []any:
		wfs := make([]Workflow, 0, len(v))
		for i, item := range v {
			obj, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("workflow %d: expected an object, got %s", i, Describe(item))
			}
			name, _ := obj["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("workflow %d: missing \"name\"", i)
			}
			wfs = append(wfs, newWorkflow(name, obj))
		}
		return wfs, nil
	case map[string]any:
		name := defaultName
		if n, ok := v["name"].(string); ok && n != "" {
			name = n
		}
		return []Workflow{newWorkflow(name, v)}, nil
	}
	return nil, fmt.Errorf("config must evaluate to a workflow object or a list of them, got %s", Describe(result))
}

func newWorkflow(name string, obj map[string]any) Workflow {
	cfg := maps.Clone(obj)
	delete(cfg, "name")
	// depends_on refers to other workflows by name, so it must see the same sanitized names.
	if deps, ok := cfg["depends_on"].([]any); ok {
		sanitized := make([]any, len(deps))
		for i, d := range deps {
			if s, ok := d.(string); ok {
				d = SanitizeName(s)
			}
			sanitized[i] = d
		}
		cfg["depends_on"] = sanitized
	}
	return Workflow{Name: SanitizeName(name), Config: cfg}
}

// Describe names the JSON type of an evaluated value, for error messages.
func Describe(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case string:
		return "a string"
	case float64, int64:
		return "a number"
	case []any:
		return "a list"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("a %T", v)
}
