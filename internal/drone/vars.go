package drone

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// compileTimeVars maps the Drone variables we can substitute at compile time to the Woodpecker
// variable with the same value.  Both systems substitute ${VAR} references in the config before
// running anything, but Woodpecker only knows CI_* names at that point: an unmapped ${DRONE_*}
// reference would silently become an empty string.
var compileTimeVars = map[string]string{
	"DRONE_BRANCH":               "CI_COMMIT_BRANCH",
	"DRONE_PULL_REQUEST":         "CI_COMMIT_PULL_REQUEST",
	"DRONE_TAG":                  "CI_COMMIT_TAG",
	"DRONE_SOURCE_BRANCH":        "CI_COMMIT_SOURCE_BRANCH",
	"DRONE_TARGET_BRANCH":        "CI_COMMIT_TARGET_BRANCH",
	"DRONE_BUILD_NUMBER":         "CI_PIPELINE_NUMBER",
	"DRONE_BUILD_PARENT":         "CI_PIPELINE_PARENT",
	"DRONE_BUILD_EVENT":          "CI_PIPELINE_EVENT",
	"DRONE_BUILD_LINK":           "CI_PIPELINE_URL",
	"DRONE_BUILD_CREATED":        "CI_PIPELINE_CREATED",
	"DRONE_BUILD_STARTED":        "CI_PIPELINE_STARTED",
	"DRONE_COMMIT":               "CI_COMMIT_SHA",
	"DRONE_COMMIT_SHA":           "CI_COMMIT_SHA",
	"DRONE_COMMIT_BEFORE":        "CI_PREV_COMMIT_SHA",
	"DRONE_COMMIT_REF":           "CI_COMMIT_REF",
	"DRONE_COMMIT_BRANCH":        "CI_COMMIT_BRANCH",
	"DRONE_COMMIT_LINK":          "CI_PIPELINE_FORGE_URL",
	"DRONE_COMMIT_MESSAGE":       "CI_COMMIT_MESSAGE",
	"DRONE_COMMIT_AUTHOR":        "CI_COMMIT_AUTHOR",
	"DRONE_COMMIT_AUTHOR_NAME":   "CI_COMMIT_AUTHOR",
	"DRONE_COMMIT_AUTHOR_EMAIL":  "CI_COMMIT_AUTHOR_EMAIL",
	"DRONE_COMMIT_AUTHOR_AVATAR": "CI_PIPELINE_AVATAR",
	"DRONE_REPO":                 "CI_REPO",
	"DRONE_REPO_OWNER":           "CI_REPO_OWNER",
	"DRONE_REPO_NAME":            "CI_REPO_NAME",
	"DRONE_REPO_LINK":            "CI_REPO_URL",
	"DRONE_REPO_BRANCH":          "CI_REPO_DEFAULT_BRANCH",
	"DRONE_REPO_PRIVATE":         "CI_REPO_PRIVATE",
	"DRONE_REMOTE_URL":           "CI_REPO_CLONE_URL",
	"DRONE_GIT_HTTP_URL":         "CI_REPO_CLONE_URL",
	"DRONE_SYSTEM_HOST":          "CI_SYSTEM_HOST",
}

// runtimeVars maps Drone variables that only have a value once a step is running on an agent to
// the name the step's shell should read instead.  Woodpecker sets the CI_* ones on the agent; the
// DRONE_STAGE_* ones are set as static step environment by the translator.
var runtimeVars = map[string]string{
	"DRONE_STAGE_MACHINE": "CI_MACHINE",
	"DRONE_WORKSPACE":     "CI_WORKSPACE",
	"DRONE_STAGE_OS":      "DRONE_STAGE_OS",
	"DRONE_STAGE_ARCH":    "DRONE_STAGE_ARCH",
}

// runtimeAliases are the remaining DRONE_* variables Drone gave every step, with the shell
// expression droneEnvCommand sets each one to.
var runtimeAliases = map[string]string{
	"DRONE_STEP_NUMBER":  `"$CI_STEP_NUMBER"`,
	"DRONE_BUILD_STATUS": `"$CI_PIPELINE_STATUS"`,
	"DRONE_REPO_SCM":     "git",
}

// droneEnvCommand exports the DRONE_* variables Drone gave every step, which the scripts Drone
// configs run read directly.  Woodpecker only sets its Drone aliases for plugin steps, not for
// commands.  They can't go in the step's environment either: Woodpecker substitutes ${VAR}s into
// the config text before parsing it, and quotes only multi-line values, so an ordinary commit
// message containing ": " would break the YAML.  Every CI_* variable is in the step's environment
// at run time, so the shell copies them instead.
var droneEnvCommand = func() string {
	values := map[string]string{}
	for d, ci := range compileTimeVars {
		values[d] = `"$` + ci + `"`
	}
	for d, rt := range runtimeVars {
		if d != rt {
			values[d] = `"$` + rt + `"`
		}
	}
	maps.Copy(values, runtimeAliases)
	var b strings.Builder
	b.WriteString("export")
	for _, d := range slices.Sorted(maps.Keys(values)) {
		b.WriteString(" " + d + "=" + values[d])
	}
	return b.String()
}()

// A run of $s followed by a DRONE_* name.  Both Drone and Woodpecker substitute only the braced
// ${NAME} form (drone/envsubst leaves a bare $NAME alone), and only after an odd number of $s,
// since $$ is an escaped literal $.  Every other form reaches the shell as a runtime reference.
var varRef = regexp.MustCompile(`(\$+)(\{?)(DRONE_[A-Za-z0-9_]+)`)

// rewriteVars rewrites DRONE_* references in s so that they expand to the same values under
// Woodpecker.  shell says whether s is shell code, where references to runtime-only values can be
// deferred to the shell; elsewhere they are an error.
func rewriteVars(s string, shell bool) (string, error) {
	var err error
	out := varRef.ReplaceAllStringFunc(s, func(m string) string {
		parts := varRef.FindStringSubmatch(m)
		dollars, brace, name := parts[1], parts[2], parts[3]
		substituted := brace == "{" && len(dollars)%2 == 1

		if substituted {
			if ci, ok := compileTimeVars[name]; ok {
				return dollars + brace + ci
			}
		}
		if rt, ok := runtimeVars[name]; ok {
			if substituted {
				if !shell {
					err = fmt.Errorf("%s is only available to commands at runtime", name)
					return m
				}
				dollars += "$"
			}
			return dollars + brace + rt
		}
		if !substituted && (runtimeAliases[name] != "" || compileTimeVars[name] != "") {
			return m
		}
		err = fmt.Errorf("unsupported Drone variable %s", name)
		return m
	})
	return out, err
}

// rewriteValue applies rewriteVars to every string within v.
func rewriteValue(v any, shell bool) (any, error) {
	switch v := v.(type) {
	case string:
		return rewriteVars(v, shell)
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			r, err := rewriteValue(e, shell)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			r, err := rewriteValue(e, shell)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = r
		}
		return out, nil
	}
	return v, nil
}

// escapeDollars makes s survive Woodpecker's variable substitution unchanged.
func escapeDollars(s string) string {
	return strings.ReplaceAll(s, "$", "$$")
}
