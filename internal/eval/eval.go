// Package eval evaluates jsonnet and Starlark pipeline configs into plain JSON-compatible values.
//
// Configs come from arbitrary commits (including pull requests from forks), so neither evaluator is
// given any access to the filesystem: jsonnet imports and Starlark load() always fail.
package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"

	"github.com/google/go-jsonnet"
	starlarkjson "go.starlark.net/lib/json"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"
)

// maxStarlarkSteps is far beyond what any reasonable config needs; it exists to stop runaway
// computation early, with the sandbox's CPU limit as the backstop.
const maxStarlarkSteps = 100_000_000

// Jsonnet evaluates a jsonnet config.  If the config's top level is a function, it is called with
// ctx as its "ctx" argument; otherwise ctx is unused.  A nil ctx passes no argument at all.
func Jsonnet(filename, src string, ctx any) (any, error) {
	vm := jsonnet.MakeVM()
	vm.Importer(noImporter{})
	if ctx != nil {
		b, err := json.Marshal(ctx)
		if err != nil {
			return nil, fmt.Errorf("encoding ctx: %w", err)
		}
		vm.TLACode("ctx", string(b))
	}
	out, err := vm.EvaluateAnonymousSnippet(filename, src)
	if err != nil {
		return nil, err
	}
	var result any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return nil, fmt.Errorf("decoding jsonnet output: %w", err)
	}
	return result, nil
}

type noImporter struct{}

func (noImporter) Import(_, importedPath string) (jsonnet.Contents, string, error) {
	return jsonnet.Contents{}, "", fmt.Errorf("cannot import %q: imports are not supported", importedPath)
}

var predeclared = starlark.StringDict{
	"struct": starlark.NewBuiltin("struct", starlarkstruct.Make),
	"json":   starlarkjson.Module,
}

// Starlark evaluates a Starlark config, which must define a main(ctx) function returning the
// config value.  ctx is exposed as nested structs, so fields are accessed as ctx.repo.name.
func Starlark(filename, src string, ctx any) (any, error) {
	thread := &starlark.Thread{
		Name: filename,
		Load: func(*starlark.Thread, string) (starlark.StringDict, error) {
			return nil, errors.New("load() is not supported")
		},
		Print: func(_ *starlark.Thread, msg string) { fmt.Fprintln(os.Stderr, msg) },
	}
	thread.SetMaxExecutionSteps(maxStarlarkSteps)

	globals, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, filename, src, predeclared)
	if err != nil {
		return nil, starlarkError(err)
	}
	main, ok := globals["main"].(starlark.Callable)
	if !ok {
		return nil, fmt.Errorf("%s: no main(ctx) function defined", filename)
	}
	sctx, err := toStarlark(ctx)
	if err != nil {
		return nil, fmt.Errorf("converting ctx: %w", err)
	}
	res, err := starlark.Call(thread, main, starlark.Tuple{sctx}, nil)
	if err != nil {
		return nil, starlarkError(err)
	}
	return fromStarlark(res)
}

func starlarkError(err error) error {
	var evalErr *starlark.EvalError
	if errors.As(err, &evalErr) {
		return errors.New(evalErr.Backtrace())
	}
	return err
}

func toStarlark(v any) (starlark.Value, error) {
	switch v := v.(type) {
	case nil:
		return starlark.None, nil
	case bool:
		return starlark.Bool(v), nil
	case string:
		return starlark.String(v), nil
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
			return starlark.MakeInt64(int64(v)), nil
		}
		return starlark.Float(v), nil
	case []any:
		elems := make([]starlark.Value, len(v))
		for i, e := range v {
			sv, err := toStarlark(e)
			if err != nil {
				return nil, err
			}
			elems[i] = sv
		}
		return starlark.NewList(elems), nil
	case map[string]any:
		fields := make(starlark.StringDict, len(v))
		for k, e := range v {
			sv, err := toStarlark(e)
			if err != nil {
				return nil, err
			}
			fields[k] = sv
		}
		return starlarkstruct.FromStringDict(starlarkstruct.Default, fields), nil
	}
	return nil, fmt.Errorf("unsupported value of type %T", v)
}

func fromStarlark(v starlark.Value) (any, error) {
	switch v := v.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.Bool:
		return bool(v), nil
	case starlark.String:
		return string(v), nil
	case starlark.Int:
		i, ok := v.Int64()
		if !ok {
			return nil, fmt.Errorf("integer %s is too large", v)
		}
		return i, nil
	case starlark.Float:
		return float64(v), nil
	case *starlark.List:
		return fromIterable(v)
	case starlark.Tuple:
		return fromIterable(v)
	case *starlark.Dict:
		m := make(map[string]any, v.Len())
		for _, item := range v.Items() {
			k, ok := item[0].(starlark.String)
			if !ok {
				return nil, fmt.Errorf("dict key %s is not a string", item[0])
			}
			e, err := fromStarlark(item[1])
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			m[string(k)] = e
		}
		return m, nil
	case *starlarkstruct.Struct:
		names := v.AttrNames()
		sort.Strings(names)
		m := make(map[string]any, len(names))
		for _, name := range names {
			attr, err := v.Attr(name)
			if err != nil {
				return nil, err
			}
			e, err := fromStarlark(attr)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			m[name] = e
		}
		return m, nil
	}
	return nil, fmt.Errorf("cannot use a %s as a config value", v.Type())
}

func fromIterable(v starlark.Indexable) ([]any, error) {
	out := make([]any, v.Len())
	for i := range out {
		e, err := fromStarlark(v.Index(i))
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		out[i] = e
	}
	return out, nil
}
