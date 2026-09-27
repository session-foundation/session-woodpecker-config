package eval

import (
	"reflect"
	"strings"
	"testing"
)

var testCtx = map[string]any{
	"repo":     map[string]any{"full_name": "session-foundation/liboxenmq"},
	"pipeline": map[string]any{"event": "push", "number": float64(42)},
}

func TestJsonnetPlain(t *testing.T) {
	got, err := Jsonnet("x.jsonnet", `[{name: 'a', steps: [{name: 's', image: 'i'}]}]`, testCtx)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{map[string]any{"name": "a", "steps": []any{map[string]any{"name": "s", "image": "i"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestJsonnetCtx(t *testing.T) {
	got, err := Jsonnet("x.jsonnet", `function(ctx) {repo: ctx.repo.full_name, n: ctx.pipeline.number}`, testCtx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"repo": "session-foundation/liboxenmq", "n": float64(42)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestJsonnetNoImports(t *testing.T) {
	for _, src := range []string{`import 'lib.libsonnet'`, `importstr '/etc/passwd'`, `importbin '/etc/passwd'`} {
		_, err := Jsonnet("x.jsonnet", src, nil)
		if err == nil || !strings.Contains(err.Error(), "imports are not supported") {
			t.Errorf("%s: expected import failure, got %v", src, err)
		}
	}
}

func TestJsonnetError(t *testing.T) {
	_, err := Jsonnet("x.jsonnet", "{\n  a: error 'boom',\n}", nil)
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "x.jsonnet:2") {
		t.Errorf("expected located error, got %v", err)
	}
}

func TestStarlark(t *testing.T) {
	src := `
def step(name):
    return {"name": name, "image": "debian"}

def main(ctx):
    return [
        {"name": ctx.repo.full_name, "steps": [step("s%d" % i) for i in range(2)]},
        struct(name = "b", n = ctx.pipeline.number, tags = ("x", None, True, 1.5)),
    ]
`
	got, err := Starlark("x.star", src, testCtx)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"name": "session-foundation/liboxenmq", "steps": []any{
			map[string]any{"name": "s0", "image": "debian"},
			map[string]any{"name": "s1", "image": "debian"},
		}},
		map[string]any{"name": "b", "n": int64(42), "tags": []any{"x", nil, true, 1.5}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestStarlarkErrors(t *testing.T) {
	for _, tc := range []struct{ src, want string }{
		{`load("lib.star", "x")`, "load() is not supported"},
		{`x = 1`, "no main(ctx) function"},
		{"def main(ctx):\n    return [x for x in range(100000) for y in range(100000)]", "too many steps"},
		{"def main(ctx):\n    return {1: 2}", "not a string"},
		{"def main(ctx):\n    return len", "cannot use a builtin_function_or_method"},
		{"def main(ctx):\n    fail('boom')", "boom"},
	} {
		_, err := Starlark("x.star", tc.src, testCtx)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: expected error containing %q, got %v", tc.src, tc.want, err)
		}
	}
}
