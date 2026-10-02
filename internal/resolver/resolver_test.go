package resolver

import (
	"path/filepath"
	"testing"
)

func TestParsePath(t *testing.T) {
	storageRoot := "/tmp/evergit"

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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePath(tt.rawPath, storageRoot)
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
