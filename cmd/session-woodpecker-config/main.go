// Command session-woodpecker-config is a Woodpecker CI configuration extension that evaluates
// jsonnet and Starlark pipeline configs (and translates legacy .drone.jsonnet ones).
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/session-foundation/session-woodpecker-config/internal/sandbox"
	"github.com/session-foundation/session-woodpecker-config/internal/server"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == sandbox.ChildArg {
		os.Exit(sandbox.RunChild(os.Args[2:]))
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:8124", "address to listen on")
	keyFile := flag.String("public-key", "", "PEM file containing the Woodpecker server's public key, as served at /api/signature/public-key (required)")
	helpURL := flag.String("help-url", "https://github.com/session-foundation/session-woodpecker-config", "URL the .drone.jsonnet deprecation notice points to")
	secretRepos := flag.String("secret-repos", "session-foundation/*", "comma-separated patterns of the repositories (owner/name) whose .drone.jsonnet pipelines are given secrets, other than for pull requests; elsewhere from_secret is dropped")
	timeout := flag.Duration("eval-timeout", 5*time.Second, "maximum time to evaluate one config file; keep well below Woodpecker's 10s extension timeout")
	memory := flag.Uint64("eval-memory", 1024, "maximum memory for evaluating one config file, in MiB")
	concurrency := flag.Int("eval-concurrency", runtime.NumCPU(), "maximum number of config files evaluated at once")
	debug := flag.Bool("debug", false, "enable debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if *keyFile == "" {
		return errors.New("-public-key is required")
	}
	pub, err := loadPublicKey(*keyFile)
	if err != nil {
		return err
	}

	runner, err := sandbox.NewRunner(sandbox.Limits{Timeout: *timeout, Memory: *memory << 20}, *concurrency)
	if err != nil {
		return err
	}
	var repos []string
	for p := range strings.SplitSeq(*secretRepos, ",") {
		if p = strings.TrimSpace(p); p != "" {
			repos = append(repos, p)
		}
	}
	handler, err := server.New(pub, runner, *helpURL, repos, log)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *listen)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func loadPublicKey(file string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("%s: no PEM public key found", file)
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%s: expected an ed25519 key, got %T", file, key)
	}
	return pub, nil
}
