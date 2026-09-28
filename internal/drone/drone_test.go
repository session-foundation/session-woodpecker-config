package drone

import (
	"flag"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/session-foundation/session-woodpecker-config/internal/eval"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestRewriteVars(t *testing.T) {
	for _, tc := range []struct {
		in    string
		shell bool
		want  string
	}{
		{`echo "Building on ${DRONE_STAGE_MACHINE}"`, true, `echo "Building on $${CI_MACHINE}"`},
		{`devname="Test-${DRONE_COMMIT:0:9}-${DRONE_BUILD_EVENT}"`, true, `devname="Test-${CI_COMMIT_SHA:0:9}-${CI_PIPELINE_EVENT}"`},
		{`cd $DRONE_WORKSPACE && yarn`, true, `cd $CI_WORKSPACE && yarn`},
		{`-DGUI_EXE=$${DRONE_WORKSPACE}/gui`, true, `-DGUI_EXE=$${CI_WORKSPACE}/gui`},
		{`echo $DRONE_TAG $$DRONE_TAG $$${DRONE_TAG}`, true, `echo $DRONE_TAG $$DRONE_TAG $$${CI_COMMIT_TAG}`},
		{`xcrun simctl boot $sim_uuid`, true, `xcrun simctl boot $sim_uuid`},
		{`upload-$${DRONE_STAGE_OS}`, true, `upload-$${DRONE_STAGE_OS}`},
		{`$(lsb_release -sc) $$(x)`, true, `$(lsb_release -sc) $$(x)`},
		{`${DRONE_TAG}`, false, `${CI_COMMIT_TAG}`},
	} {
		got, err := rewriteVars(tc.in, tc.shell)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
		} else if got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.in, got, tc.want)
		}
	}

	for _, tc := range []struct {
		in    string
		shell bool
		want  string
	}{
		{`${DRONE_STAGE_MACHINE}`, false, "only available to commands"},
		{`${DRONE_STAGE_NAME}`, true, "unsupported Drone variable DRONE_STAGE_NAME"},
		{`$$DRONE_STAGE_NAME`, true, "unsupported Drone variable DRONE_STAGE_NAME"},
		{`$DRONE_STAGE_NAME`, true, "unsupported Drone variable DRONE_STAGE_NAME"},
	} {
		if _, err := rewriteVars(tc.in, tc.shell); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: expected error containing %q, got %v", tc.in, tc.want, err)
		}
	}
}

func TestTriggers(t *testing.T) {
	for _, tc := range []struct {
		trigger any
		want    map[string]any
	}{
		{nil, map[string]any{"event": allEvents}},
		{
			map[string]any{"event": map[string]any{"exclude": []any{"pull_request"}}},
			map[string]any{"event": []string{"push", "tag", "deployment", "cron", "manual"}},
		},
		{
			map[string]any{"branch": map[string]any{"exclude": []any{"debian/*"}}},
			map[string]any{"branch": map[string]any{"exclude": []any{"debian/*"}}, "event": allEvents},
		},
		{
			map[string]any{"branch": []any{"dev"}, "event": []any{"push", "promote", "rollback"}},
			map[string]any{"branch": []any{"dev"}, "event": []string{"push", "deployment"}},
		},
	} {
		got, err := translateConstraint(tc.trigger, true)
		if err != nil {
			t.Errorf("%#v: %v", tc.trigger, err)
		} else if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%#v: got %#v, want %#v", tc.trigger, got, tc.want)
		}
	}
}

func TestNullCommands(t *testing.T) {
	wfs, err := Translate(map[string]any{"kind": "pipeline", "name": "p", "steps": []any{
		map[string]any{"name": "s", "image": "debian", "commands": []any{"a", nil, "b"}},
	}}, true)
	if err != nil {
		t.Fatal(err)
	}
	cmds := wfs[0].Config["steps"].([]any)[0].(map[string]any)["commands"]
	if !reflect.DeepEqual(cmds, []any{droneEnvCommand, "a", "b"}) {
		t.Errorf("got commands %#v", cmds)
	}
}

// TestDroneEnv runs the export command the way a step's shell would, with the values Woodpecker
// sets, including ones that would break if they were pasted into the command or the config.
func TestDroneEnv(t *testing.T) {
	message := "Fix: \"quoted\" $HOME `ls`\nsecond line"
	cmd := exec.Command("sh", "-e", "-c", droneEnvCommand+"\nprintf '%s|%s|%s|%s' \"$DRONE_COMMIT\" \"$DRONE_COMMIT_MESSAGE\" \"$DRONE_WORKSPACE\" \"$DRONE_REPO_SCM\"")
	cmd.Env = []string{"CI_COMMIT_SHA=abc123", "CI_COMMIT_MESSAGE=" + message, "CI_WORKSPACE=/woodpecker/src/x"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := "abc123|" + message + "|/woodpecker/src/x|git"; string(out) != want {
		t.Errorf("got %q, want %q", out, want)
	}
	for d := range compileTimeVars {
		if !strings.Contains(droneEnvCommand, " "+d+"=") {
			t.Errorf("%s is not exported", d)
		}
	}
}

func TestSecrets(t *testing.T) {
	secret := map[string]any{"from_secret": "SSH_KEY"}
	pipeline := map[string]any{
		"kind": "pipeline", "name": "p",
		"environment": map[string]any{"PIPELINE_KEY": secret, "A": "1"},
		"steps": []any{map[string]any{
			"name": "upload", "image": "plugins/x",
			"environment": map[string]any{"SSH_KEY": secret, "B": "2"},
			"settings":    map[string]any{"key": secret, "target": "t"},
		}},
		"services": []any{map[string]any{"name": "db", "image": "postgres", "environment": map[string]any{"PW": secret}}},
	}

	for _, tc := range []struct {
		secrets                    bool
		stepEnv, settings, service map[string]any
	}{
		{true,
			map[string]any{"PIPELINE_KEY": secret, "A": "1", "SSH_KEY": secret, "B": "2"},
			map[string]any{"key": secret, "target": "t"},
			map[string]any{"PIPELINE_KEY": secret, "A": "1", "PW": secret}},
		{false,
			map[string]any{"A": "1", "B": "2"},
			map[string]any{"target": "t"},
			map[string]any{"A": "1"}},
	} {
		wfs, err := Translate(pipeline, tc.secrets)
		if err != nil {
			t.Fatal(err)
		}
		step := wfs[0].Config["steps"].([]any)[0].(map[string]any)
		env := step["environment"].(map[string]any)
		delete(env, "DRONE_STAGE_OS")
		delete(env, "DRONE_STAGE_ARCH")
		service := wfs[0].Config["services"].([]any)[0].(map[string]any)["environment"].(map[string]any)
		delete(service, "DRONE_STAGE_OS")
		delete(service, "DRONE_STAGE_ARCH")
		if !reflect.DeepEqual(env, tc.stepEnv) {
			t.Errorf("secrets=%v: step environment %v, want %v", tc.secrets, env, tc.stepEnv)
		}
		if !reflect.DeepEqual(step["settings"], tc.settings) {
			t.Errorf("secrets=%v: settings %v, want %v", tc.secrets, step["settings"], tc.settings)
		}
		if !reflect.DeepEqual(service, tc.service) {
			t.Errorf("secrets=%v: service environment %v, want %v", tc.secrets, service, tc.service)
		}
	}
}

func TestTranslateErrors(t *testing.T) {
	step := map[string]any{"name": "build", "image": "debian", "commands": []any{"true"}}
	pipeline := func(extra map[string]any) map[string]any {
		p := map[string]any{"kind": "pipeline", "name": "p", "steps": []any{step}}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	for _, tc := range []struct {
		result any
		want   string
	}{
		{"nope", "expected a pipeline or list of pipelines"},
		{pipeline(map[string]any{"kind": "secret"}), `unsupported kind "secret"`},
		{pipeline(map[string]any{"type": "kubernetes"}), `unsupported pipeline type "kubernetes"`},
		{pipeline(map[string]any{"volumes": []any{}}), `unsupported field "volumes"`},
		{pipeline(map[string]any{"trigger": map[string]any{"target": "x"}}), `trigger: unsupported field "target"`},
		{pipeline(map[string]any{"trigger": map[string]any{"event": "pull_request_closed"}}), `unsupported event`},
		{pipeline(map[string]any{"type": "exec"}), "exec pipeline steps cannot specify an image"},
		{pipeline(map[string]any{"steps": []any{map[string]any{"name": "s", "commands": []any{"true"}}}}), `step "s": missing image`},
		{pipeline(map[string]any{"steps": []any{map[string]any{"name": "s", "image": "x", "pull": "sometimes"}}}), "unsupported pull policy"},
		{pipeline(map[string]any{"steps": []any{}}), "no steps"},
	} {
		_, err := Translate(tc.result, true)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%#v: expected error containing %q, got %v", tc.result, tc.want, err)
		}
	}
}

// TestGolden translates real Session .drone.jsonnet files; the golden files are the reviewable
// record of exactly what the translation produces.  Regenerate them with `go test -update`.
func TestGolden(t *testing.T) {
	for _, name := range []string{"oxen-mq", "session-ios"} {
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile("testdata/" + name + ".drone.jsonnet")
			if err != nil {
				t.Fatal(err)
			}
			result, err := eval.Jsonnet(name+".drone.jsonnet", string(src), nil)
			if err != nil {
				t.Fatal(err)
			}
			wfs, err := Translate(result, true)
			if err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			for _, w := range wfs {
				y, err := w.YAML()
				if err != nil {
					t.Fatal(err)
				}
				out.WriteString("# " + w.FileName() + "\n" + y + "\n")
			}

			golden := "testdata/" + name + ".golden.yaml"
			if *update {
				if err := os.WriteFile(golden, []byte(out.String()), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if out.String() != string(want) {
				t.Errorf("translation differs from %s; rerun with -update and review the diff", golden)
			}
		})
	}
}

func TestDeprecated(t *testing.T) {
	w := Deprecated("https://example.com/$x")
	y, err := w.YAML()
	if err != nil {
		t.Fatal(err)
	}
	if w.FileName() != "DEPRECATED.yaml" || !strings.Contains(y, "See https://example.com/$$x for help") {
		t.Errorf("unexpected deprecation workflow %s:\n%s", w.FileName(), y)
	}
}
