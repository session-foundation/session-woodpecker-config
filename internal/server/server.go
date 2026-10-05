// Package server implements the Woodpecker configuration extension endpoint.
//
// Woodpecker POSTs the config files it fetched for a pipeline; jsonnet and Starlark files (and a
// legacy .drone.jsonnet) are evaluated into Woodpecker workflows, and anything else is passed
// through unchanged.  If nothing needed converting the response is 204, telling Woodpecker to use
// the files as they are.  If there are any .woodpecker/override* files, only they are used.
package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/yaronf/httpsign"

	"github.com/session-foundation/session-woodpecker-config/internal/drone"
	"github.com/session-foundation/session-woodpecker-config/internal/sandbox"
	"github.com/session-foundation/session-woodpecker-config/internal/workflow"
)

// signatureName is the label Woodpecker gives its request signatures.
const signatureName = "woodpecker-ci-extensions"

// maxRequestSize bounds the request body, which holds the repo's config files.
const maxRequestSize = 16 << 20

// Evaluator evaluates a single config file; *sandbox.Runner is the real implementation.
type Evaluator interface {
	Eval(ctx context.Context, job sandbox.Job) (any, error)
}

// RepoMatcher decides which repositories (owner/name) something applies to; *repolist.File is the
// real implementation.
type RepoMatcher interface {
	Match(repo string) bool
}

// Server is the extension's HTTP handler.
type Server struct {
	verifier    *httpsign.Verifier
	eval        Evaluator
	helpURL     string
	secretRepos RepoMatcher
	log         *slog.Logger
	mux         *http.ServeMux
}

// New creates a Server that accepts requests signed by the Woodpecker server whose public key is
// pub.  helpURL is where the deprecation notice for .drone.jsonnet configs points people.
// secretRepos matches the repositories whose .drone.jsonnet pipelines are given secrets, other
// than for pull requests.
func New(pub ed25519.PublicKey, eval Evaluator, helpURL string, secretRepos RepoMatcher, log *slog.Logger) (*Server, error) {
	verifier, err := httpsign.NewEd25519Verifier(pub, httpsign.NewVerifyConfig(),
		httpsign.Headers("@request-target", "content-digest"))
	if err != nil {
		return nil, err
	}
	s := &Server{verifier: verifier, eval: eval, helpURL: helpURL, secretRepos: secretRepos, log: log, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	s.mux.HandleFunc("POST /", s.handleConfig)
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

type configFile struct {
	Name string `json:"name"`
	Data string `json:"data"`
}

type request struct {
	Repo          map[string]any `json:"repo"`
	Pipeline      map[string]any `json:"pipeline"`
	Configuration []configFile   `json:"configuration"`
}

type response struct {
	Configs []configFile `json:"configs"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestSize)
	if err := s.verify(r); err != nil {
		s.log.Warn("rejecting request with invalid signature", "remote", r.RemoteAddr, "err", err)
		http.Error(w, "invalid request signature", http.StatusUnauthorized)
		return
	}

	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	log := s.log.With("repo", req.Repo["full_name"], "pipeline", req.Pipeline["number"])

	configs, err := s.process(r.Context(), &req)
	var cfgErr *sandbox.ConfigError
	switch {
	case errors.As(err, &cfgErr):
		log.Info("config error", "err", cfgErr.Msg)
		// Woodpecker does not retry a 4xx, and shows its body as the pipeline's error.
		http.Error(w, cfgErr.Msg, http.StatusUnprocessableEntity)
	case err != nil:
		log.Error("processing config failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	case configs == nil:
		log.Debug("nothing to convert")
		w.WriteHeader(http.StatusNoContent)
	default:
		log.Info("converted config", "workflows", len(configs))
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response{Configs: configs}); err != nil {
			log.Warn("writing response failed", "err", err)
		}
	}
}

func (s *Server) verify(r *http.Request) error {
	if err := httpsign.ValidateContentDigestHeader(r.Header.Values("Content-Digest"), &r.Body,
		[]string{httpsign.DigestSha256, httpsign.DigestSha512}); err != nil {
		return err
	}
	return httpsign.VerifyRequest(signatureName, *s.verifier, r)
}

// OverridePrefix starts the names of config files in .woodpecker/ that, when there are any,
// replace all the others.  They let a branch that carries a project's own .woodpecker/ configs
// unchanged, such as a Debian packaging branch that merges from upstream, run a different set of
// workflows without conflicting with them.
const OverridePrefix = ".woodpecker/override"

// overrides returns the override files among files, or files itself if there are none.
func overrides(files []configFile) []configFile {
	var out []configFile
	for _, f := range files {
		if strings.HasPrefix(f.Name, OverridePrefix) {
			out = append(out, f)
		}
	}
	if out == nil {
		return files
	}
	return out
}

// process converts req's config files, returning nil if none of them needed converting.
func (s *Server) process(ctx context.Context, req *request) ([]configFile, error) {
	files := overrides(req.Configuration)

	buildCtx := map[string]any{"repo": req.Repo, "pipeline": req.Pipeline}
	var out []configFile
	// Dropping files the overrides replace is a change Woodpecker has to be told about, even if the
	// overrides themselves are plain YAML.
	converted := len(files) != len(req.Configuration)
	sources := map[string]string{}
	add := func(name, fileName, data, source string) error {
		if prev, ok := sources[name]; ok {
			return &sandbox.ConfigError{Msg: fmt.Sprintf("%s: workflow %q is also defined by %s", source, name, prev)}
		}
		sources[name] = source
		out = append(out, configFile{Name: fileName, Data: data})
		return nil
	}

	secrets := s.secretsAllowed(req)
	for _, f := range files {
		wfs, err := s.convert(ctx, f, buildCtx, secrets)
		if err != nil {
			return nil, err
		}
		if wfs == nil {
			name := workflow.NameFromFile(f.Name)
			fileName := f.Name
			// A restarted pipeline sends back the workflows generated for it, stored under their
			// bare names, so their order has to be recovered from the files themselves.
			if key, ok := orderKey(f.Data); ok {
				fileName = key + "/" + name + ".yaml"
				converted = true
			}
			if err := add(name, fileName, f.Data, f.Name); err != nil {
				return nil, err
			}
			continue
		}
		converted = true
		width := len(strconv.Itoa(len(wfs) - 1))
		for i, w := range wfs {
			y, err := w.YAML()
			if err != nil {
				return nil, configError(f.Name, err)
			}
			key := fmt.Sprintf("%s/%0*d", f.Name, width, i)
			if err := add(w.Name, key+"/"+w.FileName(), orderMarker+key+"\n"+y, f.Name); err != nil {
				return nil, err
			}
		}
	}

	if !converted {
		return nil, nil
	}
	if len(out) == 0 {
		return nil, &sandbox.ConfigError{Msg: "config produced no workflows"}
	}

	// A restarted pipeline already has it, among the workflows sent back.
	if event, _ := req.Pipeline["event"].(string); event == "pull_request" {
		if _, ok := sources[allBuildsName]; !ok {
			w := allBuilds(slices.Sorted(maps.Keys(sources)))
			y, err := w.YAML()
			if err != nil {
				return nil, err
			}
			if err := add(w.Name, allBuildsKey+"/"+w.FileName(), orderMarker+allBuildsKey+"\n"+y, "this service"); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// allBuildsName is the workflow that stands for a pull request's whole pipeline, so that GitHub
// can require it: GitHub can only require status checks by exact name, and Woodpecker reports one
// per workflow.
const allBuildsName = "All builds"

// allBuildsKey sorts after every other workflow's key (which starts with a config file name).
const allBuildsKey = "~"

// allBuilds is a trivial workflow that only runs once every other workflow has succeeded.  If any
// of them fails it is skipped, which leaves its status pending, so it still blocks merging.  The
// dependencies are optional so that workflows not run for this pipeline are ignored rather than
// removing this one too.
func allBuilds(names []string) workflow.Workflow {
	deps := make([]any, len(names))
	for i, n := range names {
		deps[i] = map[string]any{"name": n, "optional": true}
	}
	return workflow.Workflow{Name: allBuildsName, Config: map[string]any{
		"skip_clone": true,
		"labels":     workflow.TrivialLabels(),
		"when":       []any{map[string]any{"event": []any{"pull_request"}}},
		"depends_on": deps,
		"steps": []any{map[string]any{
			"name":     "all builds passed",
			"image":    "busybox",
			"commands": []any{"true"},
		}},
	}}
}

// Woodpecker orders a pipeline's workflows by sorting their file names, but names each workflow
// after only the file name's base.  So each generated workflow is named <key>/<name>.yaml, where
// the key is the config file and the workflow's zero-padded index in it, which keeps them in the
// order the config listed them without changing their names.
//
// Woodpecker stores the files under their bare workflow names, though, and a restart sends those
// back to be sorted again, so the key is also recorded in the file itself on a first line of
// orderMarker + key.
const orderMarker = "# session-woodpecker-config order: "

// orderKey returns the sort key recorded in a workflow file generated by this service.
func orderKey(data string) (string, bool) {
	line, _, _ := strings.Cut(data, "\n")
	key, ok := strings.CutPrefix(line, orderMarker)
	return key, ok && key != ""
}

// secretEvents are the events whose pipelines may be given secrets.  Pull requests are excluded,
// as they were by default in Drone, since their code has not been reviewed.
var secretEvents = []string{"push", "tag", "deployment", "cron", "manual"}

// secretsAllowed reports whether a translated .drone.jsonnet pipeline for req may be given
// secrets.  Woodpecker fails a pipeline outright when a secret it references is missing, so this
// has to predict where the secrets exist: a trusted event in one of the repositories they have
// been set up for.
func (s *Server) secretsAllowed(req *request) bool {
	if event, _ := req.Pipeline["event"].(string); !slices.Contains(secretEvents, event) {
		return false
	}
	repo, _ := req.Repo["full_name"].(string)
	return s.secretRepos.Match(repo)
}

// convert evaluates f into workflows, or returns nil workflows if f is not a file it converts.
func (s *Server) convert(ctx context.Context, f configFile, buildCtx any, secrets bool) ([]workflow.Workflow, error) {
	base := path.Base(f.Name)
	ext := path.Ext(base)

	if base == drone.ConfigFile {
		// Drone never passed any build context to jsonnet.
		result, err := s.eval.Eval(ctx, sandbox.Job{Lang: sandbox.Jsonnet, Filename: f.Name, Source: f.Data})
		if err != nil {
			return nil, evalError(f.Name, err)
		}
		wfs, err := drone.Translate(result, secrets)
		if err != nil {
			return nil, configError(f.Name, err)
		}
		return append([]workflow.Workflow{drone.Deprecated(s.helpURL)}, wfs...), nil
	}

	var lang sandbox.Lang
	switch ext {
	case ".jsonnet":
		lang = sandbox.Jsonnet
	case ".star":
		lang = sandbox.Starlark
	default:
		return nil, nil
	}
	result, err := s.eval.Eval(ctx, sandbox.Job{Lang: lang, Filename: f.Name, Source: f.Data, Context: buildCtx})
	if err != nil {
		return nil, evalError(f.Name, err)
	}
	wfs, err := workflow.FromResult(result, strings.TrimPrefix(strings.TrimSuffix(base, ext), "."))
	if err != nil {
		return nil, configError(f.Name, err)
	}
	return wfs, nil
}

// evalError attributes an evaluation failure to its config file.  Failures that were not the
// config's fault (such as a crashed child process) are left as internal errors.
func evalError(file string, err error) error {
	var cfgErr *sandbox.ConfigError
	if errors.As(err, &cfgErr) {
		return &sandbox.ConfigError{Msg: file + ": " + cfgErr.Msg}
	}
	return fmt.Errorf("%s: %w", file, err)
}

// configError reports that the config file produced something unusable.
func configError(file string, err error) error {
	return &sandbox.ConfigError{Msg: file + ": " + err.Error()}
}
