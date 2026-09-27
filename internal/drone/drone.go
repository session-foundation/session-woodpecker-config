// Package drone translates the pipelines produced by a legacy .drone.jsonnet into Woodpecker
// workflows.
//
// Only the subset of Drone's pipeline format that Session projects actually use is supported.
// Anything else is an error rather than a best guess, so that a translated pipeline never silently
// does something different from what it did under Drone.
package drone

import (
	"fmt"
	"maps"
	"slices"

	"github.com/session-foundation/session-woodpecker-config/internal/workflow"
)

// ConfigFile is the name of the legacy Drone config the translator handles.
const ConfigFile = ".drone.jsonnet"

// Translate converts the evaluated output of a .drone.jsonnet (a pipeline object or a list of
// them) into Woodpecker workflows.
func Translate(result any) ([]workflow.Workflow, error) {
	var pipelines []any
	switch v := result.(type) {
	case []any:
		pipelines = v
	case map[string]any:
		pipelines = []any{v}
	default:
		return nil, fmt.Errorf("expected a pipeline or list of pipelines, got %s", workflow.Describe(result))
	}

	wfs := make([]workflow.Workflow, 0, len(pipelines))
	for i, p := range pipelines {
		obj, ok := p.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("pipeline %d: expected an object, got %s", i, workflow.Describe(p))
		}
		name, _ := obj["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("pipeline %d: missing \"name\"", i)
		}
		cfg, err := translatePipeline(obj)
		if err != nil {
			return nil, fmt.Errorf("pipeline %q: %w", name, err)
		}
		wfs = append(wfs, workflow.Workflow{Name: workflow.SanitizeName(name), Config: cfg})
	}
	return wfs, nil
}

var pipelineFields = []string{"kind", "type", "name", "platform", "node", "environment", "services", "steps", "trigger", "depends_on"}

func translatePipeline(p map[string]any) (map[string]any, error) {
	if err := checkFields(p, pipelineFields); err != nil {
		return nil, err
	}
	if kind, _ := p["kind"].(string); kind != "pipeline" {
		return nil, fmt.Errorf("unsupported kind %q", p["kind"])
	}

	var backend string
	switch typ, _ := p["type"].(string); typ {
	case "", "docker":
		backend = "docker"
	case "exec":
		backend = "local"
	default:
		return nil, fmt.Errorf("unsupported pipeline type %q", typ)
	}

	os, arch := "linux", "amd64"
	if plat, ok := p["platform"]; ok {
		pm, ok := plat.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("platform: expected an object")
		}
		if err := checkFields(pm, []string{"os", "arch"}); err != nil {
			return nil, fmt.Errorf("platform: %w", err)
		}
		if s, ok := pm["os"].(string); ok {
			os = s
		}
		if s, ok := pm["arch"].(string); ok {
			arch = s
		}
	}

	labels := map[string]any{"platform": os + "/" + arch, "backend": backend}
	if node, ok := p["node"]; ok {
		nm, ok := node.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("node: expected an object")
		}
		for k, v := range nm {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("node: %s: expected a string", k)
			}
			labels[k] = s
		}
	}

	env, err := objectField(p, "environment")
	if err != nil {
		return nil, err
	}
	// Scripts from the Drone era read these, and Woodpecker has no equivalent step variables.
	env = mergeEnv(map[string]any{"DRONE_STAGE_OS": os, "DRONE_STAGE_ARCH": arch}, env)

	cfg := map[string]any{"labels": labels}

	when, err := translateConstraint(p["trigger"], true)
	if err != nil {
		return nil, fmt.Errorf("trigger: %w", err)
	}
	cfg["when"] = []any{when}

	steps, err := translateList(p, "steps", func(s map[string]any) (map[string]any, error) {
		return translateStep(s, backend, env)
	})
	if err != nil {
		return nil, err
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("no steps")
	}
	cfg["steps"] = steps

	services, err := translateList(p, "services", func(s map[string]any) (map[string]any, error) {
		return translateService(s, env)
	})
	if err != nil {
		return nil, err
	}
	if len(services) > 0 {
		cfg["services"] = services
	}

	if deps, ok := p["depends_on"]; ok {
		names, err := stringList(deps)
		if err != nil {
			return nil, fmt.Errorf("depends_on: %w", err)
		}
		sanitized := make([]any, len(names))
		for i, n := range names {
			sanitized[i] = workflow.SanitizeName(n)
		}
		cfg["depends_on"] = sanitized
	}
	return cfg, nil
}

var stepFields = []string{"name", "image", "commands", "environment", "pull", "failure", "depends_on", "when", "settings", "detach", "privileged"}

func translateStep(s map[string]any, backend string, pipelineEnv map[string]any) (map[string]any, error) {
	if err := checkFields(s, stepFields); err != nil {
		return nil, err
	}
	out := map[string]any{"name": s["name"]}

	switch image, _ := s["image"].(string); {
	case backend == "local" && image != "":
		return nil, fmt.Errorf("exec pipeline steps cannot specify an image")
	case backend == "local":
		// Woodpecker's local backend runs commands with the step's "image" as the shell; Drone's exec
		// runner used /bin/sh -e, which is what the local backend does for "sh".
		out["image"] = "sh"
	case image == "":
		return nil, fmt.Errorf("missing image")
	default:
		img, err := rewriteVars(image, false)
		if err != nil {
			return nil, fmt.Errorf("image: %w", err)
		}
		out["image"] = img
	}

	if err := translateCommon(s, out, pipelineEnv); err != nil {
		return nil, err
	}

	if f, ok := s["failure"]; ok {
		if f != "ignore" {
			return nil, fmt.Errorf("unsupported failure mode %v", f)
		}
		out["failure"] = "ignore"
	}
	if deps, ok := s["depends_on"]; ok {
		names, err := stringList(deps)
		if err != nil {
			return nil, fmt.Errorf("depends_on: %w", err)
		}
		out["depends_on"] = names
	}
	if w, ok := s["when"]; ok {
		when, err := translateConstraint(w, false)
		if err != nil {
			return nil, fmt.Errorf("when: %w", err)
		}
		out["when"] = []any{when}
	}
	if settings, ok := s["settings"]; ok {
		v, err := rewriteValue(settings, false)
		if err != nil {
			return nil, fmt.Errorf("settings: %w", err)
		}
		out["settings"] = v
	}
	for _, k := range []string{"detach", "privileged"} {
		if v, ok := s[k]; ok {
			b, ok := v.(bool)
			if !ok {
				return nil, fmt.Errorf("%s: expected a boolean", k)
			}
			out[k] = b
		}
	}
	return out, nil
}

var serviceFields = []string{"name", "image", "commands", "environment", "pull"}

func translateService(s map[string]any, pipelineEnv map[string]any) (map[string]any, error) {
	if err := checkFields(s, serviceFields); err != nil {
		return nil, err
	}
	image, _ := s["image"].(string)
	if image == "" {
		return nil, fmt.Errorf("missing image")
	}
	image, err := rewriteVars(image, false)
	if err != nil {
		return nil, fmt.Errorf("image: %w", err)
	}
	out := map[string]any{"name": s["name"], "image": image}
	if err := translateCommon(s, out, pipelineEnv); err != nil {
		return nil, err
	}
	return out, nil
}

// translateCommon handles the fields steps and services share.
func translateCommon(s, out map[string]any, pipelineEnv map[string]any) error {
	if cmds, ok := s["commands"]; ok {
		// Drone ran a null command as an empty line, and configs rely on that: jsonnet's
		// `[if cond then cmd]` produces [null] when cond is false.
		if l, ok := cmds.([]any); ok {
			cmds = slices.DeleteFunc(slices.Clone(l), func(c any) bool { return c == nil })
		}
		list, err := stringList(cmds)
		if err != nil {
			return fmt.Errorf("commands: %w", err)
		}
		rewritten := make([]any, len(list))
		for i, c := range list {
			if rewritten[i], err = rewriteVars(c, true); err != nil {
				return fmt.Errorf("commands: %w", err)
			}
		}
		out["commands"] = rewritten
	}

	env, err := objectField(s, "environment")
	if err != nil {
		return err
	}
	env = mergeEnv(pipelineEnv, env)
	for k, v := range env {
		switch v := v.(type) {
		case string:
			if env[k], err = rewriteVars(v, false); err != nil {
				return fmt.Errorf("environment: %s: %w", k, err)
			}
		case map[string]any:
			if len(v) != 1 || v["from_secret"] == nil {
				return fmt.Errorf("environment: %s: only from_secret is supported", k)
			}
		}
	}
	if len(env) > 0 {
		out["environment"] = env
	}

	switch pull := s["pull"]; pull {
	case nil, "if-not-exists", "never":
	case "always":
		out["pull"] = true
	default:
		return fmt.Errorf("unsupported pull policy %v", pull)
	}
	return nil
}

// allEvents are the Woodpecker events equivalent to Drone's events.  Drone never ran pipelines for
// closed pull requests or for GitHub releases (which also create a tag event), so neither is here.
var allEvents = []string{"push", "pull_request", "tag", "deployment", "cron", "manual"}

var eventMap = map[string]string{
	"push":         "push",
	"pull_request": "pull_request",
	"tag":          "tag",
	"promote":      "deployment",
	"rollback":     "deployment",
	"cron":         "cron",
	"custom":       "manual",
}

// translateConstraint converts a Drone trigger or step "when" into a Woodpecker when constraint.
// Pipeline triggers always get an explicit event list, as Woodpecker warns about workflows that
// have none.
func translateConstraint(c any, pipeline bool) (map[string]any, error) {
	var cm map[string]any
	if c != nil {
		var ok bool
		if cm, ok = c.(map[string]any); !ok {
			return nil, fmt.Errorf("expected an object")
		}
	}
	if err := checkFields(cm, []string{"branch", "event", "ref", "repo", "cron", "status"}); err != nil {
		return nil, err
	}

	out := map[string]any{}
	for _, k := range []string{"branch", "ref", "repo", "cron"} {
		if v, ok := cm[k]; ok {
			if err := checkCondition(v); err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = v
		}
	}
	if v, ok := cm["status"]; ok {
		statuses, err := stringList(v)
		if err != nil {
			return nil, fmt.Errorf("status: %w", err)
		}
		out["status"] = statuses
	}

	if v, ok := cm["event"]; ok {
		events, err := translateEvents(v)
		if err != nil {
			return nil, fmt.Errorf("event: %w", err)
		}
		out["event"] = events
	} else if pipeline {
		out["event"] = slices.Clone(allEvents)
	}
	return out, nil
}

func translateEvents(v any) ([]string, error) {
	include, exclude, err := condition(v)
	if err != nil {
		return nil, err
	}
	mapEvents := func(names []string) ([]string, error) {
		var out []string
		for _, n := range names {
			e, ok := eventMap[n]
			if !ok {
				return nil, fmt.Errorf("unsupported event %q", n)
			}
			if !slices.Contains(out, e) {
				out = append(out, e)
			}
		}
		return out, nil
	}
	inc, err := mapEvents(include)
	if err != nil {
		return nil, err
	}
	exc, err := mapEvents(exclude)
	if err != nil {
		return nil, err
	}
	// Woodpecker's event filter has no exclude form, so exclusions become the complementary list.
	if len(inc) == 0 {
		inc = slices.Clone(allEvents)
	}
	return slices.DeleteFunc(inc, func(e string) bool { return slices.Contains(exc, e) }), nil
}

// checkCondition verifies a Drone condition is a shape Woodpecker's list conditions also accept.
func checkCondition(v any) error {
	_, _, err := condition(v)
	return err
}

// condition parses a Drone condition: a string, a list of strings, or an include/exclude object.
func condition(v any) (include, exclude []string, err error) {
	if m, ok := v.(map[string]any); ok {
		if err := checkFields(m, []string{"include", "exclude"}); err != nil {
			return nil, nil, err
		}
		if i, ok := m["include"]; ok {
			if include, err = stringList(i); err != nil {
				return nil, nil, fmt.Errorf("include: %w", err)
			}
		}
		if e, ok := m["exclude"]; ok {
			if exclude, err = stringList(e); err != nil {
				return nil, nil, fmt.Errorf("exclude: %w", err)
			}
		}
		return include, exclude, nil
	}
	include, err = stringList(v)
	return include, nil, err
}

func translateList(p map[string]any, field string, f func(map[string]any) (map[string]any, error)) ([]any, error) {
	v, ok := p[field]
	if !ok {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: expected a list", field)
	}
	out := make([]any, len(list))
	for i, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s[%d]: expected an object", field, i)
		}
		name, _ := obj["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("%s[%d]: missing \"name\"", field, i)
		}
		t, err := f(obj)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", field[:len(field)-1], name, err)
		}
		out[i] = t
	}
	return out, nil
}

func checkFields(m map[string]any, allowed []string) error {
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !slices.Contains(allowed, k) {
			return fmt.Errorf("unsupported field %q", k)
		}
	}
	return nil
}

func objectField(m map[string]any, field string) (map[string]any, error) {
	v, ok := m[field]
	if !ok {
		return nil, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: expected an object", field)
	}
	return obj, nil
}

// mergeEnv returns base overlaid with over, without modifying either.
func mergeEnv(base, over map[string]any) map[string]any {
	out := maps.Clone(base)
	if out == nil {
		out = map[string]any{}
	}
	maps.Copy(out, over)
	return out
}

func stringList(v any) ([]string, error) {
	if s, ok := v.(string); ok {
		return []string{s}, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("expected a string or list of strings, got %s", workflow.Describe(v))
	}
	out := make([]string, len(list))
	for i, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("expected a string or list of strings, got %s in list", workflow.Describe(e))
		}
		out[i] = s
	}
	return out, nil
}

// Deprecated returns a trivial workflow whose only purpose is to show up in the pipeline and in
// the forge's commit status checks, telling people to migrate away from .drone.jsonnet.
func Deprecated(helpURL string) workflow.Workflow {
	return workflow.Workflow{
		Name: "DEPRECATED",
		Config: map[string]any{
			"skip_clone": true,
			"labels":     map[string]any{"backend": "docker"},
			"when":       []any{map[string]any{"event": slices.Clone(allEvents)}},
			"steps": []any{map[string]any{
				"name":  "Drone-CI deprecated",
				"image": "busybox",
				"commands": []any{
					"echo '" + ConfigFile + " is deprecated; please migrate to .woodpecker/build.jsonnet'",
					"echo 'See " + escapeDollars(helpURL) + " for help'",
				},
			}},
		},
	}
}
