// Package server implements the Woodpecker configuration extension endpoint.
//
// Woodpecker POSTs the config files it fetched for a pipeline; jsonnet and Starlark files (and a
// legacy .drone.jsonnet) are evaluated into Woodpecker workflows, and anything else is passed
// through unchanged.  If nothing needed converting the response is 204, telling Woodpecker to use
// the files as they are.
package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"slices"
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

// process converts req's config files, returning nil if none of them needed converting.
func (s *Server) process(ctx context.Context, req *request) ([]configFile, error) {
	buildCtx := map[string]any{"repo": req.Repo, "pipeline": req.Pipeline}
	var out []configFile
	converted := false
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
	for _, f := range req.Configuration {
		wfs, err := s.convert(ctx, f, buildCtx, secrets)
		if err != nil {
			return nil, err
		}
		if wfs == nil {
			if err := add(workflow.NameFromFile(f.Name), f.Name, f.Data, f.Name); err != nil {
				return nil, err
			}
			continue
		}
		converted = true
		for _, w := range wfs {
			y, err := w.YAML()
			if err != nil {
				return nil, configError(f.Name, err)
			}
			if err := add(w.Name, w.FileName(), y, f.Name); err != nil {
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
	return out, nil
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
