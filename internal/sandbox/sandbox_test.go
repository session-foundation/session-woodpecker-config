package sandbox

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as the evaluation child, as the real binary does.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == ChildArg {
		os.Exit(RunChild(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func runner(t *testing.T, limits Limits) *Runner {
	t.Helper()
	r, err := NewRunner(limits, 2)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var defaultLimits = Limits{Timeout: 5 * time.Second, Memory: 1 << 30}

func TestEval(t *testing.T) {
	r := runner(t, defaultLimits)
	ctx := map[string]any{"repo": map[string]any{"name": "liboxenmq"}}

	got, err := r.Eval(context.Background(), Job{Lang: Jsonnet, Filename: "x.jsonnet", Source: `function(ctx) [ctx.repo.name]`, Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []any{"liboxenmq"}) {
		t.Errorf("jsonnet: got %#v", got)
	}

	got, err = r.Eval(context.Background(), Job{Lang: Starlark, Filename: "x.star", Source: "def main(ctx):\n    return [ctx.repo.name]", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []any{"liboxenmq"}) {
		t.Errorf("starlark: got %#v", got)
	}
}

func expectConfigError(t *testing.T, err error, want string) {
	t.Helper()
	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("expected a ConfigError containing %q, got %v", want, err)
	}
	if !strings.Contains(cfgErr.Msg, want) {
		t.Errorf("expected error containing %q, got %q", want, cfgErr.Msg)
	}
}

func TestConfigError(t *testing.T) {
	_, err := runner(t, defaultLimits).Eval(context.Background(), Job{Lang: Jsonnet, Filename: "x.jsonnet", Source: `error 'boom'`})
	expectConfigError(t, err, "boom")
}

func TestTimeout(t *testing.T) {
	// Jsonnet has no loop that runs in constant memory (even tailstrict recursion grows the Go
	// stack), so this uses a Starlark loop that would take far longer than the timeout to exhaust
	// its step limit.
	src := "def main(ctx):\n    x = 0\n    for i in range(1000000000):\n        x += i\n    return x"
	start := time.Now()
	_, err := runner(t, Limits{Timeout: 300 * time.Millisecond, Memory: 1 << 30}).Eval(context.Background(), Job{Lang: Starlark, Filename: "x.star", Source: src})
	expectConfigError(t, err, "timed out")
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("timeout took %s to take effect", elapsed)
	}
}

func TestMemoryLimit(t *testing.T) {
	r := runner(t, Limits{Timeout: 30 * time.Second, Memory: 512 << 20})
	for _, src := range []string{
		`std.length(std.makeArray(1e9, function(i) {i: i}))`,
		`local loop(n) = if n == 0 then 0 else loop(n - 1) tailstrict; loop(1e12)`,
	} {
		_, err := r.Eval(context.Background(), Job{Lang: Jsonnet, Filename: "x.jsonnet", Source: src})
		expectConfigError(t, err, "memory limit")
	}
}
