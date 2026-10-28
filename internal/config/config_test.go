package config

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseShorthands(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected map[string]string
	}{
		{
			name:     "Empty string",
			input:    "",
			expected: map[string]string{},
		},
		{
			name:     "Explicit none",
			input:    "none",
			expected: map[string]string{},
		},
		{
			name:  "Standard pairs",
			input: "github=github.com,gitlab=gitlab.com,bitbucket=bitbucket.org",
			expected: map[string]string{
				"github":    "github.com",
				"gitlab":    "gitlab.com",
				"bitbucket": "bitbucket.org",
			},
		},
		{
			name:  "Whitespace and case insensitivity",
			input: " GH = GitHub.com , GL=GitLab.com , invalid_no_equals ",
			expected: map[string]string{
				"gh": "github.com",
				"gl": "gitlab.com",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseShorthands(tt.input)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("ParseShorthands(%q) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}

func TestLoadShorthandsFromEnv(t *testing.T) {
	t.Setenv("EVERGIT_SHORTHANDS", "custom=custom.example.com")
	cfg := Load()
	want := map[string]string{"custom": "custom.example.com"}
	if !reflect.DeepEqual(cfg.Shorthands, want) {
		t.Errorf("cfg.Shorthands = %v, want %v", cfg.Shorthands, want)
	}
}

func TestLoadMaxSubmoduleDepth(t *testing.T) {
	cases := map[string]int{"": DefaultMaxSubmoduleDepth, "0": 0, "7": 7, "-1": DefaultMaxSubmoduleDepth, "x": DefaultMaxSubmoduleDepth}
	for env, want := range cases {
		t.Setenv("EVERGIT_MAX_SUBMODULE_DEPTH", env)
		if got := Load().MaxSubmoduleDepth; got != want {
			t.Errorf("EVERGIT_MAX_SUBMODULE_DEPTH=%q: MaxSubmoduleDepth = %d, want %d", env, got, want)
		}
	}
}

func TestLoadStorageRootHierarchy(t *testing.T) {
	t.Run("Default fallback", func(t *testing.T) {
		t.Setenv("EVERGIT_STORAGE_ROOT", "")
		t.Setenv("STATE_DIRECTORY", "")
		cfg := Load()
		want, _ := filepath.Abs("./storage")
		if cfg.StorageRoot != want {
			t.Errorf("StorageRoot = %q, want %q", cfg.StorageRoot, want)
		}
	})

	t.Run("StateDirectory fallback", func(t *testing.T) {
		t.Setenv("EVERGIT_STORAGE_ROOT", "")
		t.Setenv("STATE_DIRECTORY", "/var/lib/evergit")
		cfg := Load()
		if cfg.StorageRoot != "/var/lib/evergit" {
			t.Errorf("StorageRoot = %q, want %q", cfg.StorageRoot, "/var/lib/evergit")
		}
	})

	t.Run("EvergitStorageRoot takes precedence over StateDirectory", func(t *testing.T) {
		t.Setenv("EVERGIT_STORAGE_ROOT", "/data/evergit")
		t.Setenv("STATE_DIRECTORY", "/var/lib/evergit")
		cfg := Load()
		if cfg.StorageRoot != "/data/evergit" {
			t.Errorf("StorageRoot = %q, want %q", cfg.StorageRoot, "/data/evergit")
		}
	})
}

func TestFormatShorthands(t *testing.T) {
	if got := FormatShorthands(nil); got != "none" {
		t.Errorf("FormatShorthands(nil) = %q, want 'none'", got)
	}
	if got := FormatShorthands(map[string]string{}); got != "none" {
		t.Errorf("FormatShorthands(empty) = %q, want 'none'", got)
	}
	m := map[string]string{
		"gl": "gitlab.com",
		"gh": "github.com",
	}
	want := "gh=github.com, gl=gitlab.com"
	if got := FormatShorthands(m); got != want {
		t.Errorf("FormatShorthands(%v) = %q, want %q", m, got, want)
	}
}
