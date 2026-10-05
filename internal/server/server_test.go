package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/yaronf/httpsign"

	"github.com/session-foundation/session-woodpecker-config/internal/eval"
	"github.com/session-foundation/session-woodpecker-config/internal/sandbox"
	"github.com/session-foundation/session-woodpecker-config/internal/workflow"
)

// directEval evaluates in-process; the sandbox has its own tests.
type directEval struct{}

func (directEval) Eval(_ context.Context, job sandbox.Job) (any, error) {
	var res any
	var err error
	if job.Lang == sandbox.Starlark {
		res, err = eval.Starlark(job.Filename, job.Source, job.Context)
	} else {
		res, err = eval.Jsonnet(job.Filename, job.Source, job.Context)
	}
	if err != nil {
		return nil, &sandbox.ConfigError{Msg: err.Error()}
	}
	return res, nil
}

type ownerMatcher string

func (o ownerMatcher) Match(repo string) bool {
	return strings.HasPrefix(repo, string(o)+"/")
}

type fixture struct {
	url    string
	client *httpsign.Client
}

func setup(t *testing.T) *fixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(pub, directEval{}, "https://example.com/help", ownerMatcher("session-foundation"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	// This mirrors how Woodpecker signs extension requests (server/services/utils/http.go).
	signer, err := httpsign.NewEd25519Signer(priv, httpsign.NewSignConfig(), httpsign.Headers("@request-target", "content-digest"))
	if err != nil {
		t.Fatal(err)
	}
	client := httpsign.NewClient(http.Client{}, httpsign.NewClientConfig().SetSignatureName(signatureName).SetSigner(signer))
	return &fixture{url: ts.URL + "/config", client: client}
}

func (f *fixture) post(t *testing.T, files ...configFile) (int, string) {
	t.Helper()
	return f.postFor(t, "session-foundation/liboxenmq", "push", files...)
}

func (f *fixture) postFor(t *testing.T, repo, event string, files ...configFile) (int, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"repo":          map[string]any{"full_name": repo},
		"pipeline":      map[string]any{"number": 7, "event": event},
		"configuration": files,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.client.Post(f.url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func decodeConfigs(t *testing.T, body string) []configFile {
	t.Helper()
	var resp response
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	return resp.Configs
}

func names(configs []configFile) []string {
	out := make([]string, len(configs))
	for i, c := range configs {
		out[i] = c.Name
	}
	return out
}

func TestSignatureRequired(t *testing.T) {
	f := setup(t)
	body := `{"configuration":[{"name":"x.jsonnet","data":"{}"}]}`

	resp, err := http.Post(f.url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unsigned request: got status %d", resp.StatusCode)
	}

	// A validly signed request whose body is swapped afterwards must fail the digest check.
	req, err := http.NewRequest(http.MethodPost, f.url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	signed, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	signed.Body.Close()
	if signed.StatusCode != http.StatusOK {
		t.Fatalf("signed request: got status %d", signed.StatusCode)
	}
	tampered, err := http.NewRequest(http.MethodPost, f.url, strings.NewReader(strings.Replace(body, "{}", "[]", 1)))
	if err != nil {
		t.Fatal(err)
	}
	tampered.Header = signed.Request.Header.Clone()
	if tampered.Header.Get("Signature") == "" || tampered.Header.Get("Content-Digest") == "" {
		t.Fatalf("signed request headers lack a signature: %v", tampered.Header)
	}
	resp, err = http.DefaultClient.Do(tampered)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("tampered request: got status %d", resp.StatusCode)
	}
}

func TestPassThrough(t *testing.T) {
	status, _ := setup(t).post(t, configFile{".woodpecker.yaml", "steps: []"})
	if status != http.StatusNoContent {
		t.Errorf("got status %d, want 204", status)
	}
}

func TestMixedDirectory(t *testing.T) {
	status, body := setup(t).post(t,
		configFile{".woodpecker/build.jsonnet", `function(ctx) [{name: 'Debian sid/' + ctx.pipeline.event, steps: []}]`},
		configFile{".woodpecker/docs.yaml", "steps: []\n"},
		configFile{".woodpecker/lint.star", "def main(ctx):\n    return {\"steps\": [{\"name\": ctx.repo.full_name}]}"},
	)
	if status != http.StatusOK {
		t.Fatalf("got status %d: %s", status, body)
	}
	configs := decodeConfigs(t, body)
	want := []string{".woodpecker/build.jsonnet/0/Debian sid: push.yaml", ".woodpecker/docs.yaml", ".woodpecker/lint.star/0/lint.yaml"}
	if got := names(configs); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got configs %q, want %q", got, want)
	}
	if configs[1].Data != "steps: []\n" {
		t.Errorf("passed-through config was modified: %q", configs[1].Data)
	}
	if !strings.Contains(configs[2].Data, "session-foundation/liboxenmq") {
		t.Errorf("starlark config did not see ctx: %q", configs[2].Data)
	}
}

func TestOverride(t *testing.T) {
	status, body := setup(t).post(t,
		configFile{".woodpecker/build.star", "def main(ctx):\n    return [{\"name\": \"upstream\", \"steps\": []}]"},
		configFile{".woodpecker/docs.yaml", "steps: []\n"},
		configFile{".woodpecker/override-lint.jsonnet", "{name: 'lint', steps: []}"},
		configFile{".woodpecker/override.star", "def main(ctx):\n    return [{\"name\": \"Debian sid\", \"steps\": []}]"},
	)
	if status != http.StatusOK {
		t.Fatalf("got status %d: %s", status, body)
	}
	want := []string{".woodpecker/override-lint.jsonnet/0/lint.yaml", ".woodpecker/override.star/0/Debian sid.yaml"}
	if got := names(decodeConfigs(t, body)); !slices.Equal(got, want) {
		t.Errorf("got configs %q, want %q", got, want)
	}
}

// TestOverrideYAML checks that YAML overrides, which need no converting, still replace the other
// files rather than being passed through along with them.
func TestOverrideYAML(t *testing.T) {
	status, body := setup(t).post(t,
		configFile{".woodpecker/build.star", "def main(ctx):\n    return [{\"name\": \"upstream\", \"steps\": []}]"},
		configFile{".woodpecker/override.yaml", "steps: []\n"},
	)
	if status != http.StatusOK {
		t.Fatalf("got status %d: %s", status, body)
	}
	want := []string{".woodpecker/override.yaml"}
	if got := names(decodeConfigs(t, body)); !slices.Equal(got, want) {
		t.Errorf("got configs %q, want %q", got, want)
	}
}

// TestOrder checks that sorting the file names, as Woodpecker does, keeps the order the config
// listed its workflows in, and that the names Woodpecker derives from the files are unchanged.
func TestOrder(t *testing.T) {
	var src strings.Builder
	src.WriteString("[")
	var want []string
	for i := range 12 {
		name := fmt.Sprintf("%c", 'z'-i)
		want = append(want, name)
		fmt.Fprintf(&src, "{name: '%s', steps: []},", name)
	}
	src.WriteString("]")
	f := setup(t)
	check := func(what string, configs []configFile) {
		t.Helper()
		files := names(configs)
		slices.Sort(files)
		got := make([]string, len(files))
		for i, f := range files {
			got[i] = workflow.NameFromFile(f)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: got workflows %q in sorted order, want %q (files %q)", what, got, want, files)
		}
	}

	status, body := f.post(t, configFile{".woodpecker/build.jsonnet", src.String()})
	if status != http.StatusOK {
		t.Fatalf("got status %d: %s", status, body)
	}
	configs := decodeConfigs(t, body)
	check("first run", configs)

	// A restart sends the stored workflows back under their bare names, in no particular order.
	stored := make([]configFile, len(configs))
	for i, c := range configs {
		stored[i] = configFile{workflow.NameFromFile(c.Name), c.Data}
	}
	slices.Reverse(stored)
	status, body = f.post(t, stored...)
	if status != http.StatusOK {
		t.Fatalf("restart: got status %d: %s", status, body)
	}
	restarted := decodeConfigs(t, body)
	check("restart", restarted)
	for i, c := range restarted {
		if c.Data != stored[i].Data {
			t.Errorf("restart: workflow %q was modified", c.Name)
		}
	}
}

func TestAllBuilds(t *testing.T) {
	f := setup(t)
	src := configFile{".woodpecker/build.jsonnet", "[{name: 'b', steps: []}, {name: 'a', steps: []}]"}

	status, body := f.postFor(t, "session-foundation/libquic", "push", src)
	if status != http.StatusOK {
		t.Fatalf("push: got status %d: %s", status, body)
	}
	for _, c := range decodeConfigs(t, body) {
		if workflow.NameFromFile(c.Name) == allBuildsName {
			t.Errorf("push pipeline got %q", c.Name)
		}
	}

	status, body = f.postFor(t, "session-foundation/libquic", "pull_request", src)
	if status != http.StatusOK {
		t.Fatalf("pull_request: got status %d: %s", status, body)
	}
	configs := decodeConfigs(t, body)
	files := names(configs)
	slices.Sort(files)
	if got := workflow.NameFromFile(files[len(files)-1]); len(files) != 3 || got != allBuildsName {
		t.Fatalf("expected the two workflows then %q in sorted order, got %q", allBuildsName, files)
	}
	var all configFile
	for _, c := range configs {
		if workflow.NameFromFile(c.Name) == allBuildsName {
			all = c
		}
	}
	for _, want := range []string{"- name: a\n      optional: true", "- name: b\n      optional: true", "- pull_request"} {
		if !strings.Contains(all.Data, want) {
			t.Errorf("%s lacks %q:\n%s", allBuildsName, want, all.Data)
		}
	}

	// A restart sends it back along with the others, and it must not be added twice.
	stored := make([]configFile, len(configs))
	for i, c := range configs {
		stored[i] = configFile{workflow.NameFromFile(c.Name), c.Data}
	}
	status, body = f.postFor(t, "session-foundation/libquic", "pull_request", stored...)
	if status != http.StatusOK {
		t.Fatalf("restart: got status %d: %s", status, body)
	}
	files = names(decodeConfigs(t, body))
	slices.Sort(files)
	if got := workflow.NameFromFile(files[len(files)-1]); len(files) != 3 || got != allBuildsName {
		t.Errorf("restart: expected the two workflows then %q in sorted order, got %q", allBuildsName, files)
	}
}

func TestDrone(t *testing.T) {
	status, body := setup(t).post(t, configFile{".drone.jsonnet", `[{kind: 'pipeline', name: 'Debian sid (amd64)', steps: [{name: 'build', image: 'debian', commands: ['echo ${DRONE_COMMIT}']}]}]`})
	if status != http.StatusOK {
		t.Fatalf("got status %d: %s", status, body)
	}
	configs := decodeConfigs(t, body)
	if got := names(configs); len(got) != 2 || got[0] != ".drone.jsonnet/0/DEPRECATED.yaml" || got[1] != ".drone.jsonnet/1/Debian sid (amd64).yaml" {
		t.Fatalf("got configs %q", got)
	}
	if !strings.Contains(configs[0].Data, "https://example.com/help") {
		t.Errorf("deprecation notice lacks help URL: %s", configs[0].Data)
	}
	if !strings.Contains(configs[1].Data, "echo ${CI_COMMIT_SHA}") {
		t.Errorf("Drone variable not rewritten: %s", configs[1].Data)
	}
}

func TestDroneSecrets(t *testing.T) {
	f := setup(t)
	src := configFile{".drone.jsonnet", `{kind: 'pipeline', name: 'p', steps: [{name: 's', image: 'debian', environment: {SSH_KEY: {from_secret: 'SSH_KEY'}}}]}`}
	for _, tc := range []struct {
		repo, event string
		secrets     bool
	}{
		{"session-foundation/liboxenmq", "push", true},
		{"session-foundation/liboxenmq", "tag", true},
		{"session-foundation/liboxenmq", "pull_request", false},
		{"session-foundation/liboxenmq", "pull_request_closed", false},
		{"jagerman/loki-mq", "push", false},
		{"session-foundation-fork/liboxenmq", "push", false},
	} {
		status, body := f.postFor(t, tc.repo, tc.event, src)
		if status != http.StatusOK {
			t.Fatalf("%s %s: got status %d: %s", tc.repo, tc.event, status, body)
		}
		if got := strings.Contains(decodeConfigs(t, body)[1].Data, "from_secret"); got != tc.secrets {
			t.Errorf("%s %s: got secrets %v, want %v", tc.repo, tc.event, got, tc.secrets)
		}
	}
}

func TestConfigErrors(t *testing.T) {
	f := setup(t)
	for _, tc := range []struct {
		files []configFile
		want  string
	}{
		{[]configFile{{".woodpecker/build.jsonnet", "{\n  a: error 'boom',\n}"}}, ".woodpecker/build.jsonnet: "},
		{[]configFile{{".woodpecker/build.jsonnet", "42"}}, "got a number"},
		{[]configFile{{".woodpecker/build.jsonnet", "[]"}}, "no workflows"},
		{[]configFile{{".drone.jsonnet", "[{kind: 'secret', name: 'x'}]"}}, `.drone.jsonnet: pipeline "x": unsupported kind`},
		{
			[]configFile{{".woodpecker/a.jsonnet", "[{name: 'docs'}]"}, {".woodpecker/docs.yaml", "steps: []"}},
			`.woodpecker/docs.yaml: workflow "docs" is also defined by .woodpecker/a.jsonnet`,
		},
	} {
		status, body := f.post(t, tc.files...)
		if status != http.StatusUnprocessableEntity || !strings.Contains(body, tc.want) {
			t.Errorf("%v: got status %d %q, want 422 containing %q", tc.files, status, body, tc.want)
		}
	}
}

func TestHealthz(t *testing.T) {
	f := setup(t)
	resp, err := http.Get(strings.TrimSuffix(f.url, "/config") + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("got status %d", resp.StatusCode)
	}
}
