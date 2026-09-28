package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yaronf/httpsign"
)

// TestEndToEnd runs the real binary, including evaluation in sandboxed child processes, against
// requests signed the way Woodpecker signs them.
func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "session-woodpecker-config")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}

	addr := freeAddr(t)
	cmd := exec.Command(bin, "-listen", addr, "-public-key", keyFile, "-secret-repos", filepath.Join(dir, "secret-repos"))
	var logs bytes.Buffer
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			t.Logf("server logs:\n%s", logs.String())
		}
	})
	waitHealthy(t, "http://"+addr+"/healthz")

	signer, err := httpsign.NewEd25519Signer(priv, httpsign.NewSignConfig(), httpsign.Headers("@request-target", "content-digest"))
	if err != nil {
		t.Fatal(err)
	}
	client := httpsign.NewClient(http.Client{}, httpsign.NewClientConfig().SetSignatureName("woodpecker-ci-extensions").SetSigner(signer))

	droneSrc, err := os.ReadFile("../../internal/drone/testdata/oxen-mq.drone.jsonnet")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		file, src   string
		status      int
		workflows   int
		bodyContent string
	}{
		{".drone.jsonnet", string(droneSrc), http.StatusOK, 16, "DEPRECATED.yaml"},
		{".woodpecker.star", "def main(ctx):\n    return {\"steps\": [{\"name\": ctx.repo.full_name, \"image\": \"x\"}]}", http.StatusOK, 1, "session-foundation/liboxenmq"},
		{".woodpecker.jsonnet", "std.makeArray(1e9, function(i) {i: i})", http.StatusUnprocessableEntity, 0, "memory limit"},
	} {
		body, err := json.Marshal(map[string]any{
			"repo":          map[string]any{"full_name": "session-foundation/liboxenmq"},
			"pipeline":      map[string]any{"number": 1},
			"configuration": []map[string]string{{"name": tc.file, "data": tc.src}},
		})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Post("http://"+addr+"/config", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status || !strings.Contains(string(respBody), tc.bodyContent) {
			t.Errorf("%s: got status %d %.200q, want %d containing %q", tc.file, resp.StatusCode, respBody, tc.status, tc.bodyContent)
			continue
		}
		if tc.status == http.StatusOK {
			var out struct{ Configs []any }
			if err := json.Unmarshal(respBody, &out); err != nil {
				t.Fatal(err)
			}
			if len(out.Configs) != tc.workflows {
				t.Errorf("%s: got %d workflows, want %d", tc.file, len(out.Configs), tc.workflows)
			}
		}
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func waitHealthy(t *testing.T, url string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
	}
	t.Fatal("server did not become healthy")
}
