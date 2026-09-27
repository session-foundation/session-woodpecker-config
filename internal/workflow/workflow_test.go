package workflow

import (
	"reflect"
	"strings"
	"testing"
)

func TestSanitizeName(t *testing.T) {
	for in, want := range map[string]string{
		"Debian sid (amd64)":          "Debian sid (amd64)",
		"Debian sid/clang-19 (amd64)": "Debian sid: clang-19 (amd64)",
		"a / b/c ":                    "a: b: c",
	} {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNameFromFile(t *testing.T) {
	for in, want := range map[string]string{
		".woodpecker.yaml":       "woodpecker",
		".woodpecker/build.yml":  "build",
		".woodpecker/lint.yaml":  "lint",
		"Debian sid (i386).yaml": "Debian sid (i386)",
	} {
		if got := NameFromFile(in); got != want {
			t.Errorf("NameFromFile(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFromResultList(t *testing.T) {
	result := []any{
		map[string]any{"name": "Debian sid/Debug", "steps": []any{}},
		map[string]any{"name": "after", "depends_on": []any{"Debian sid/Debug"}},
	}
	got, err := FromResult(result, "build")
	if err != nil {
		t.Fatal(err)
	}
	want := []Workflow{
		{Name: "Debian sid: Debug", Config: map[string]any{"steps": []any{}}},
		{Name: "after", Config: map[string]any{"depends_on": []any{"Debian sid: Debug"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
	if _, ok := result[0].(map[string]any)["name"]; !ok {
		t.Error("FromResult modified its input")
	}
}

func TestFromResultSingle(t *testing.T) {
	got, err := FromResult(map[string]any{"steps": []any{}}, "build")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "build" {
		t.Errorf("got %#v", got)
	}
}

func TestFromResultErrors(t *testing.T) {
	for _, tc := range []struct {
		result any
		want   string
	}{
		{"x", "got a string"},
		{[]any{float64(1)}, "workflow 0: expected an object, got a number"},
		{[]any{map[string]any{"steps": []any{}}}, `workflow 0: missing "name"`},
	} {
		_, err := FromResult(tc.result, "build")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%#v: expected error containing %q, got %v", tc.result, tc.want, err)
		}
	}
}

func TestYAML(t *testing.T) {
	w := Workflow{Name: "x", Config: map[string]any{
		"steps": []any{map[string]any{"name": "build", "commands": []any{"echo $${CI_MACHINE}"}, "pull": true}},
	}}
	got, err := w.YAML()
	if err != nil {
		t.Fatal(err)
	}
	want := "steps:\n    - commands:\n        - echo $${CI_MACHINE}\n      name: build\n      pull: true\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
