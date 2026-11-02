package annot_test

import (
	"strings"
	"testing"

	"github.com/truvity/ocictl/pkg/commands/annot"
)

func TestSourceFromRemote(t *testing.T) {
	for remote, want := range map[string]string{
		"https://github.com/example-org/repo.git":              "https://github.com/example-org/repo",
		"https://github.com/example-org/repo":                  "https://github.com/example-org/repo",
		"https://user:secret@github.com/example-org/repo.git":  "https://github.com/example-org/repo",
		"https://x-access-token:abc@git.example.com:8443/o/r/": "https://git.example.com:8443/o/r",
		"ssh://git@github.com/example-org/repo.git":            "https://github.com/example-org/repo",
		"ssh://git@git.example.com:2222/example-org/repo":      "https://git.example.com/example-org/repo",
		"git@github.com:example-org/repo.git":                  "https://github.com/example-org/repo",
		"github.com:example-org/repo":                          "https://github.com/example-org/repo",
	} {
		got, err := annot.SourceFromRemote(remote)
		if err != nil || got != want {
			t.Errorf("SourceFromRemote(%q) = %q, %v; want %q", remote, got, err, want)
		}

		if got != "" && (strings.Contains(got, "secret") || strings.Contains(got, "abc@")) {
			t.Errorf("credentials leaked: %q", got)
		}
	}

	for _, bad := range []string{"", "/srv/git/repo.git", "https://", "git@host:"} {
		if got, err := annot.SourceFromRemote(bad); err == nil {
			t.Errorf("SourceFromRemote(%q) = %q, want an error", bad, got)
		}
	}
}

func TestMerge(t *testing.T) {
	if annot.Merge(nil, nil) != nil {
		t.Fatal("two empty maps must merge to nil")
	}

	got := annot.Merge(map[string]string{"a": "1", "b": "2"}, map[string]string{"b": "3"})
	if len(got) != 2 || got["a"] != "1" || got["b"] != "3" {
		t.Fatalf("got %v", got)
	}
}
