package resolver

import (
	"path/filepath"
	"testing"

	"evergit/internal/config"
)

func TestParsePath(t *testing.T) {
	storageRoot := "/tmp/evergit"
	cfg := &config.Config{StorageRoot: storageRoot, AllowedHosts: config.DefaultAllowedHosts}

	tests := []struct {
		name        string
		rawPath     string
		wantDomain  string
		wantOwner   string
		wantRepo    string
		wantRemote  string
		wantMirror  string
		wantServing string
		wantErr     bool
	}{
		{
			name:        "Shorthand github",
			rawPath:     "/github/ta/evergit.git",
			wantDomain:  "github.com",
			wantOwner:   "ta",
			wantRepo:    "evergit",
			wantRemote:  "https://github.com/ta/evergit.git",
			wantMirror:  filepath.Join(storageRoot, "mirrors", "github.com", "ta", "evergit.git"),
			wantServing: filepath.Join(storageRoot, "repos", "github.com", "ta", "evergit.git"),
			wantErr:     false,
		},
		{
			name:        "GitLab subgroup",
			rawPath:     "gitlab.com/org/sub1/sub2/myrepo",
			wantDomain:  "gitlab.com",
			wantOwner:   "org/sub1/sub2",
			wantRepo:    "myrepo",
			wantRemote:  "https://gitlab.com/org/sub1/sub2/myrepo.git",
			wantMirror:  filepath.Join(storageRoot, "mirrors", "gitlab.com", "org", "sub1", "sub2", "myrepo.git"),
			wantServing: filepath.Join(storageRoot, "repos", "gitlab.com", "org", "sub1", "sub2", "myrepo.git"),
			wantErr:     false,
		},
		{
			name:    "Too short",
			rawPath: "github/ta",
			wantErr: true,
		},
		{
			name:    "Invalid domain",
			rawPath: "invalidDomain/ta/repo",
			wantErr: true,
		},
		{
			name:        "Dot-prefixed repo name",
			rawPath:     "github/org/.github",
			wantDomain:  "github.com",
			wantOwner:   "org",
			wantRepo:    ".github",
			wantRemote:  "https://github.com/org/.github.git",
			wantMirror:  filepath.Join(storageRoot, "mirrors", "github.com", "org", ".github.git"),
			wantServing: filepath.Join(storageRoot, "repos", "github.com", "org", ".github.git"),
		},
		{
			name:    "Relative traversal escaping storage",
			rawPath: "../../../tmp/pwn/x/y.git",
			wantErr: true,
		},
		{
			name:    "Traversal behind a valid domain",
			rawPath: "a.b/../../../../tmp/x/y",
			wantErr: true,
		},
		{
			name:    "Traversal inside owner",
			rawPath: "github.com/org/../other/repo",
			wantErr: true,
		},
		{
			name:    "Bare .git repo name",
			rawPath: "github.com/org/project/.git",
			wantErr: true,
		},
		{
			name:    "Empty segment",
			rawPath: "github.com/org//repo",
			wantErr: true,
		},
		{
			name:        "Host is case-insensitive",
			rawPath:     "GitHub.com/ta/evergit",
			wantDomain:  "github.com",
			wantOwner:   "ta",
			wantRepo:    "evergit",
			wantRemote:  "https://github.com/ta/evergit.git",
			wantMirror:  filepath.Join(storageRoot, "mirrors", "github.com", "ta", "evergit.git"),
			wantServing: filepath.Join(storageRoot, "repos", "github.com", "ta", "evergit.git"),
		},
		{
			name:    "Host not in allowlist",
			rawPath: "evil.test/a/b",
			wantErr: true,
		},
		{
			name:    "IP address host",
			rawPath: "169.254.169.254/latest/meta-data/x",
			wantErr: true,
		},
		{
			name:    "Host with port",
			rawPath: "127.0.0.1:9200/a/b",
			wantErr: true,
		},
		{
			name:    "Host with userinfo",
			rawPath: "user:pass@github.com/a/b",
			wantErr: true,
		},
		{
			name:    "Unsafe characters",
			rawPath: "github.com/org/re po",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePath(tt.rawPath, cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParsePath() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			if got.Domain != tt.wantDomain {
				t.Errorf("Domain = %q, want %q", got.Domain, tt.wantDomain)
			}
			if got.Owner != tt.wantOwner {
				t.Errorf("Owner = %q, want %q", got.Owner, tt.wantOwner)
			}
			if got.RepoName != tt.wantRepo {
				t.Errorf("RepoName = %q, want %q", got.RepoName, tt.wantRepo)
			}
			if got.RemoteURL != tt.wantRemote {
				t.Errorf("RemoteURL = %q, want %q", got.RemoteURL, tt.wantRemote)
			}
			if got.MirrorPath != tt.wantMirror {
				t.Errorf("MirrorPath = %q, want %q", got.MirrorPath, tt.wantMirror)
			}
			if got.ServingPath != tt.wantServing {
				t.Errorf("ServingPath = %q, want %q", got.ServingPath, tt.wantServing)
			}
		})
	}
}
