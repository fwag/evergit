package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"evergit/internal/config"
	"evergit/internal/converter"
	"evergit/internal/server"
)

// The aliases documented in the README, verbatim.
const (
	aliasFromSHA1 = `!f() { base=$(git config evergit.url || git remote get-url origin); curl -fsS "${base%/}/sha1/$1"; }; f`
	aliasToSHA1   = `!f() { base=$(git config evergit.url || git remote get-url origin); curl -fsS "${base%/}/sha256/$(git rev-parse "${1:-HEAD}")"; }; f`
)

func TestIDLookup(t *testing.T) {
	tempDir := t.TempDir()
	upstreamPath := filepath.Join(tempDir, "upstream")
	if _, err := runCmd("", "git", "init", "-q", "--initial-branch=main", "--object-format=sha1", upstreamPath); err != nil {
		t.Fatal(err)
	}
	_, _ = runCmd(upstreamPath, "git", "config", "user.name", "Test User")
	_, _ = runCmd(upstreamPath, "git", "config", "user.email", "test@example.com")
	_ = os.WriteFile(filepath.Join(upstreamPath, "f.txt"), []byte("hello"), 0644)
	_, _ = runCmd(upstreamPath, "git", "add", "f.txt")
	if _, err := runCmd(upstreamPath, "git", "commit", "-q", "-m", "initial"); err != nil {
		t.Fatal(err)
	}
	sha1, _ := runCmd(upstreamPath, "git", "rev-parse", "HEAD")

	cfg := &config.Config{
		StorageRoot:       filepath.Join(tempDir, "storage"),
		CacheTTL:          5 * time.Minute,
		AllowedHosts:      []string{"local.test"},
		UpstreamOverrides: map[string]string{"local.test": upstreamPath},
	}
	httpServer, err := server.NewHTTPServer(cfg, converter.NewManager(cfg.CacheTTL))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(httpServer)
	defer ts.Close()
	repoURL := ts.URL + "/local.test/test/repo.git"

	get := func(path string) (int, string) {
		resp, err := http.Get(repoURL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(body))
	}

	status, sha256 := get("/sha1/" + sha1)
	if status != http.StatusOK || len(sha256) != 64 {
		t.Fatalf("sha1 lookup: status %d, body %q", status, sha256)
	}
	servingHead, _ := runCmd(filepath.Join(cfg.StorageRoot, "repos", "local.test", "test", "repo.git"), "git", "--git-dir=.", "rev-parse", "HEAD")
	if sha256 != servingHead {
		t.Errorf("sha1 lookup returned %s, want serving HEAD %s", sha256, servingHead)
	}

	if status, body := get("/sha1/" + sha1[:7]); status != http.StatusOK || body != sha256 {
		t.Errorf("abbreviated sha1 lookup: status %d, body %q", status, body)
	}
	if status, body := get("/sha256/" + sha256); status != http.StatusOK || body != sha1 {
		t.Errorf("sha256 lookup: status %d, body %q, want %s", status, body, sha1)
	}
	if status, _ := get("/sha1/" + strings.Repeat("0", 40)); status != http.StatusNotFound {
		t.Errorf("unknown id: status %d, want 404", status)
	}
	if status, _ := get("/sha1/abc"); status != http.StatusBadRequest {
		t.Errorf("too-short id: status %d, want 400", status)
	}
	if status, _ := get("/sha1/" + strings.Repeat("z", 40)); status != http.StatusBadRequest {
		t.Errorf("non-hex id: status %d, want 400", status)
	}

	// End to end: the README aliases in a plain clone, without compat mode
	clone := filepath.Join(tempDir, "clone")
	if _, err := runCmd("", "git", "clone", "-q", repoURL, clone); err != nil {
		t.Fatal(err)
	}
	_, _ = runCmd(clone, "git", "config", "alias.from-sha1", aliasFromSHA1)
	_, _ = runCmd(clone, "git", "config", "alias.to-sha1", aliasToSHA1)

	if out, err := runCmd(clone, "git", "from-sha1", sha1[:7]); err != nil || out != sha256 {
		t.Errorf("git from-sha1 = %q (err %v), want %s", out, err, sha256)
	}
	if out, err := runCmd(clone, "git", "to-sha1"); err != nil || out != sha1 {
		t.Errorf("git to-sha1 = %q (err %v), want %s", out, err, sha1)
	}
	if _, err := runCmd(clone, "git", "checkout", "-q", "--detach", sha256); err != nil {
		t.Errorf("checkout of translated id failed: %v", err)
	}

	// SSH clones point evergit.url at the HTTP endpoint instead
	_, _ = runCmd(clone, "git", "remote", "set-url", "origin", "ssh://git@127.0.0.1:1/local.test/test/repo.git")
	_, _ = runCmd(clone, "git", "config", "evergit.url", repoURL)
	if out, err := runCmd(clone, "git", "to-sha1", "HEAD"); err != nil || out != sha1 {
		t.Errorf("git to-sha1 with evergit.url = %q (err %v), want %s", out, err, sha1)
	}
}

func TestCloneOfRepoWithSubmodulesExplainsFailure(t *testing.T) {
	tempDir := t.TempDir()
	upstreamPath := filepath.Join(tempDir, "upstream")
	_, _ = runCmd("", "git", "init", "-q", "--initial-branch=main", "--object-format=sha1", upstreamPath)
	_, _ = runCmd(upstreamPath, "git", "config", "user.name", "Test User")
	_, _ = runCmd(upstreamPath, "git", "config", "user.email", "test@example.com")
	_, _ = runCmd(upstreamPath, "git", "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("ab", 20)+",libs/dependency")
	if _, err := runCmd(upstreamPath, "git", "commit", "-q", "-m", "add submodule"); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		StorageRoot:       filepath.Join(tempDir, "storage"),
		CacheTTL:          5 * time.Minute,
		AllowedHosts:      []string{"local.test"},
		UpstreamOverrides: map[string]string{"local.test": upstreamPath},
	}
	httpServer, err := server.NewHTTPServer(cfg, converter.NewManager(cfg.CacheTTL))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(httpServer)
	defer ts.Close()

	_, err = runCmd("", "git", "clone", "-q", ts.URL+"/local.test/test/repo.git", filepath.Join(tempDir, "clone"))
	if err == nil || !strings.Contains(err.Error(), "submodule") {
		t.Errorf("clone should fail explaining submodules are unsupported, got: %v", err)
	}
}
