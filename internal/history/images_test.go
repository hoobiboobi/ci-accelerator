package history

import (
	"reflect"
	"testing"
)

func TestRepository(t *testing.T) {
	for in, want := range map[string]string{
		"registry.example.com/group/app:1.2":      "registry.example.com/group/app",
		"registry.example.com:5000/group/app:1.2": "registry.example.com:5000/group/app",
		"registry.example.com:5000/group/app":     "registry.example.com:5000/group/app",
		"group/app@sha256:0123":                   "group/app",
		"group/app:1.2@sha256:0123":               "group/app",
		"localhost:5000/x":                        "localhost:5000/x",
		"app":                                     "app",
		"":                                        "",
	} {
		if got := Repository(in); got != want {
			t.Errorf("Repository(%q) = %q, want %q", in, got, want)
		}
	}
	got := Repositories([]string{"r/a:1", " r/a:2 ", "r/b:1", ""})
	if !reflect.DeepEqual(got, []string{"r/a", "r/b"}) {
		t.Errorf("Repositories: %q", got)
	}
}

// An image exporter's name attribute holds several names joined with
// commas: the CI component pushes "<repo>:<commit>,<repo>:latest" (a
// v0.34.1 record keeps the attribute as given, 2026-10-28).
func TestRepositoriesOfJoinedNames(t *testing.T) {
	for _, tc := range []struct {
		in          []string
		names, reps []string
	}{
		{[]string{"registry.example.com:5000/g/app:abc,registry.example.com:5000/g/app:latest"},
			[]string{"registry.example.com:5000/g/app:abc", "registry.example.com:5000/g/app:latest"},
			[]string{"registry.example.com:5000/g/app"}},
		{[]string{"reg/g/app@sha256:0123, reg/g/app:1.2@sha256:4567 ,reg/g/other:x"},
			[]string{"reg/g/app@sha256:0123", "reg/g/app:1.2@sha256:4567", "reg/g/other:x"},
			[]string{"reg/g/app", "reg/g/other"}},
		{[]string{"reg/b:1,", ",, reg/a:2", "reg/b:3"},
			[]string{"reg/b:1", "reg/a:2", "reg/b:3"},
			[]string{"reg/b", "reg/a"}},
		{[]string{"localhost:5000/x,localhost:5000/y:1"},
			[]string{"localhost:5000/x", "localhost:5000/y:1"},
			[]string{"localhost:5000/x", "localhost:5000/y"}},
		{[]string{",", " "}, nil, nil},
		{nil, nil, nil},
	} {
		if got := ImageNames(tc.in); !reflect.DeepEqual(got, tc.names) {
			t.Errorf("ImageNames(%q) = %q, want %q", tc.in, got, tc.names)
		}
		if got := Repositories(tc.in); !reflect.DeepEqual(got, tc.reps) {
			t.Errorf("Repositories(%q) = %q, want %q", tc.in, got, tc.reps)
		}
	}
}
