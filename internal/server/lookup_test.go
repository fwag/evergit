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
	aliasFromSHA1 = `!f() { id="$1"; r="${2:-$(git config evergit.remote || true)}"; base=$(git config evergit.url || true); rem=""; if [ -n "$r" ]; then rem=$(git remote get-url "$r" 2>/dev/null || true); fi; if [ -z "$rem" ]; then for cand in $(git remote 2>/dev/null); do curl_u=$(git remote get-url "$cand" 2>/dev/null || true); if [[ "$curl_u" == *evergit* ]]; then rem="$curl_u"; break; fi; done; fi; if [ -z "$rem" ]; then rem=$(git remote get-url upstream 2>/dev/null || git remote get-url origin 2>/dev/null || true); fi; if [ -z "$base" ]; then if [[ "$rem" =~ ^https?://.* ]]; then base=$(echo "$rem" | grep -oE '^[a-zA-Z0-9+.-]+://[^/]+'); else base="https://evergit.suse.de"; fi; fi; if [[ "$base" == */*.*/*/* ]]; then u="${base%/}"; else if [ -z "$rem" ]; then echo "Error: no valid remote found (specify remote: git from-sha1 <id> [remote])" >&2; return 1; fi; p=$(echo "$rem" | sed -E "s#^[a-zA-Z0-9+.-]+://[^/]+/##; s#^[a-zA-Z0-9._-]+@[^:]+:##; s#^/##"); u="${base%/}/${p%/}"; fi; curl -fsS "$u/sha1/$id"; }; f`
	aliasToSHA1   = `!f() { arg1="$1"; arg2="$2"; rev="HEAD"; r=""; if [ -n "$arg2" ]; then rev="${arg1:-HEAD}"; r="$arg2"; elif [ -n "$arg1" ]; then if git remote get-url "$arg1" >/dev/null 2>&1 && ! git rev-parse --verify --quiet "$arg1^{commit}" >/dev/null 2>&1; then r="$arg1"; else rev="$arg1"; fi; fi; [ -z "$r" ] && r=$(git config evergit.remote || true); base=$(git config evergit.url || true); rem=""; if [ -n "$r" ]; then rem=$(git remote get-url "$r" 2>/dev/null || true); fi; if [ -z "$rem" ]; then for cand in $(git remote 2>/dev/null); do curl_u=$(git remote get-url "$cand" 2>/dev/null || true); if [[ "$curl_u" == *evergit* ]]; then rem="$curl_u"; break; fi; done; fi; if [ -z "$rem" ]; then rem=$(git remote get-url upstream 2>/dev/null || git remote get-url origin 2>/dev/null || true); fi; if [ -z "$base" ]; then if [[ "$rem" =~ ^https?://.* ]]; then base=$(echo "$rem" | grep -oE '^[a-zA-Z0-9+.-]+://[^/]+'); else base="https://evergit.suse.de"; fi; fi; if [[ "$base" == */*.*/*/* ]]; then u="${base%/}"; else if [ -z "$rem" ]; then echo "Error: no valid remote found (specify remote: git to-sha1 [rev] [remote])" >&2; return 1; fi; p=$(echo "$rem" | sed -E "s#^[a-zA-Z0-9+.-]+://[^/]+/##; s#^[a-zA-Z0-9._-]+@[^:]+:##; s#^/##"); u="${base%/}/${p%/}"; fi; curl -fsS "$u/sha256/$(git rev-parse "$rev")"; }; f`
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

	// Test multi-remote / subtree / fork setup:
	// origin points to downstream forge (e.g. gitea@src.suse.de:owner/repo.git)
	// upstream points to evergit SSH (e.g. ssh://git@127.0.0.1:1/local.test/test/repo.git)
	_, _ = runCmd(clone, "git", "config", "--unset", "evergit.url")
	_, _ = runCmd(clone, "git", "remote", "set-url", "origin", "gitea@src.suse.de:owner/repo.git")
	_, _ = runCmd(clone, "git", "remote", "add", "upstream", "ssh://git@127.0.0.1:1/local.test/test/repo.git")
	_, _ = runCmd(clone, "git", "config", "evergit.url", ts.URL)

	// 1. Explicit remote argument
	if out, err := runCmd(clone, "git", "from-sha1", sha1[:7], "upstream"); err != nil || out != sha256 {
		t.Errorf("git from-sha1 with explicit remote = %q (err %v), want %s", out, err, sha256)
	}
	if out, err := runCmd(clone, "git", "to-sha1", "HEAD", "upstream"); err != nil || out != sha1 {
		t.Errorf("git to-sha1 with explicit remote = %q (err %v), want %s", out, err, sha1)
	}

	// 2. Auto-detection (upstream fallback over origin)
	if out, err := runCmd(clone, "git", "from-sha1", sha1[:7]); err != nil || out != sha256 {
		t.Errorf("git from-sha1 auto-detecting upstream = %q (err %v), want %s", out, err, sha256)
	}
	if out, err := runCmd(clone, "git", "to-sha1"); err != nil || out != sha1 {
		t.Errorf("git to-sha1 auto-detecting upstream = %q (err %v), want %s", out, err, sha1)
	}
	if out, err := runCmd(clone, "git", "to-sha1", "upstream"); err != nil || out != sha1 {
		t.Errorf("git to-sha1 passing remote as single argument = %q (err %v), want %s", out, err, sha1)
	}

	// 3. evergit.remote configuration
	_, _ = runCmd(clone, "git", "config", "evergit.remote", "upstream")
	if out, err := runCmd(clone, "git", "from-sha1", sha1[:7]); err != nil || out != sha256 {
		t.Errorf("git from-sha1 with evergit.remote = %q (err %v), want %s", out, err, sha256)
	}
	if out, err := runCmd(clone, "git", "to-sha1"); err != nil || out != sha1 {
		t.Errorf("git to-sha1 with evergit.remote = %q (err %v), want %s", out, err, sha1)
	}
}

func TestCloneOfRepoWithSubmodulesNeedsApproval(t *testing.T) {
	tempDir := t.TempDir()
	upstreamPath := filepath.Join(tempDir, "upstream")
	_, _ = runCmd("", "git", "init", "-q", "--initial-branch=main", "--object-format=sha1", upstreamPath)
	_, _ = runCmd(upstreamPath, "git", "config", "user.name", "Test User")
	_, _ = runCmd(upstreamPath, "git", "config", "user.email", "test@example.com")
	_, _ = runCmd(upstreamPath, "git", "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("ab", 20)+",libs/dependency")
	if _, err := runCmd(upstreamPath, "git", "commit", "-q", "-m", "add submodule"); err != nil {
		t.Fatal(err)
	}

	approvals := filepath.Join(tempDir, "synthetic-gitlinks")
	cfg := &config.Config{
		StorageRoot:           filepath.Join(tempDir, "storage"),
		CacheTTL:              5 * time.Minute,
		AllowedHosts:          []string{"local.test"},
		UpstreamOverrides:     map[string]string{"local.test": upstreamPath},
		SyntheticGitlinksFile: approvals,
	}
	conv := converter.NewManager(cfg.CacheTTL)
	conv.SetConfig(cfg)
	httpServer, err := server.NewHTTPServer(cfg, conv)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(httpServer)
	defer ts.Close()

	// The gitlink has no URL: the clone fails, telling the user which approval the administrator needs
	cloneDir := filepath.Join(tempDir, "clone")
	_, err = runCmd("", "git", "clone", "-q", ts.URL+"/local.test/test/repo.git", cloneDir)
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "local.test/test/repo path:libs/dependency") {
		t.Fatalf("clone of repo with an unapproved submodule = %v, want a 403 suggesting the approval line", err)
	}

	// Approved by the administrator, without restarting
	if err := os.WriteFile(approvals, []byte("local.test/test/repo path:libs/dependency\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = runCmd("", "git", "clone", "-q", ts.URL+"/local.test/test/repo.git", cloneDir)
	if err != nil {
		t.Fatalf("clone of repo with approved submodule failed: %v", err)
	}
	format, err := runCmd(cloneDir, "git", "rev-parse", "--show-object-format")
	if err != nil || strings.TrimSpace(format) != "sha256" {
		t.Errorf("cloned repository should be sha256, got: %s (err %v)", format, err)
	}
}

func TestLoadSheddingReturns503(t *testing.T) {
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

	cfg := &config.Config{
		StorageRoot:       filepath.Join(tempDir, "storage"),
		CacheTTL:          0,
		AllowedHosts:      []string{"github.com"},
		Shorthands:        config.ParseShorthands(config.DefaultShorthands),
		UpstreamOverrides: map[string]string{"github.com": upstreamPath},
	}

	conv := converter.NewManager(0, 1)
	conv.SetConversionTimeout(10 * time.Millisecond)

	// Acquire the conversion slot
	if !conv.TryAcquireConversionSlot() {
		t.Fatal("expected slot acquisition to succeed")
	}
	defer conv.ReleaseConversionSlot()

	httpServer, err := server.NewHTTPServer(cfg, conv)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/github.com/owner/repo.git/info/refs?service=git-upload-pack", nil)
	rec := httptest.NewRecorder()
	httpServer.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if retryAfter := rec.Header().Get("Retry-After"); retryAfter != "5" {
		t.Errorf("Retry-After = %q, want %q", retryAfter, "5")
	}
}

func TestUnavailableSubmoduleReturns503WithRetryHint(t *testing.T) {
	tempDir := t.TempDir()
	upstreamPath := filepath.Join(tempDir, "upstream")
	if _, err := runCmd("", "git", "init", "-q", "--initial-branch=main", "--object-format=sha1", upstreamPath); err != nil {
		t.Fatal(err)
	}
	_, _ = runCmd(upstreamPath, "git", "config", "user.name", "Test User")
	_, _ = runCmd(upstreamPath, "git", "config", "user.email", "test@example.com")
	_, _ = runCmd(upstreamPath, "git", "config", "-f", ".gitmodules", "submodule.sub.path", "sub")
	_, _ = runCmd(upstreamPath, "git", "config", "-f", ".gitmodules", "submodule.sub.url", "https://sub.test/owner/sub.git")
	_, _ = runCmd(upstreamPath, "git", "add", ".gitmodules")
	_, _ = runCmd(upstreamPath, "git", "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("ab", 20)+",sub")
	if _, err := runCmd(upstreamPath, "git", "commit", "-q", "-m", "add submodule"); err != nil {
		t.Fatal(err)
	}
	// The submodule upstream cannot be read, as with a network error
	unreadable := filepath.Join(tempDir, "unreadable")
	if err := os.WriteFile(unreadable, nil, 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		StorageRoot:       filepath.Join(tempDir, "storage"),
		AllowedHosts:      []string{"github.com", "sub.test"},
		Shorthands:        config.ParseShorthands(config.DefaultShorthands),
		UpstreamOverrides: map[string]string{"github.com": upstreamPath, "sub.test": unreadable},
	}
	conv := converter.NewManager(0)
	conv.SetConfig(cfg)
	httpServer, err := server.NewHTTPServer(cfg, conv)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/github.com/owner/repo.git/info/refs?service=git-upload-pack", nil)
	rec := httptest.NewRecorder()
	httpServer.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header")
	}
	if body := rec.Body.String(); !strings.Contains(body, "sub.test") || !strings.Contains(body, "retry later") {
		t.Errorf("body should name the submodule and suggest retrying, got: %q", body)
	}
}

func TestLongConversionReturns503AndFinishesInBackground(t *testing.T) {
	tempDir := t.TempDir()
	upstreamPath := filepath.Join(tempDir, "upstream")
	if _, err := runCmd("", "git", "init", "-q", "--initial-branch=main", "--object-format=sha1", upstreamPath); err != nil {
		t.Fatal(err)
	}
	_, _ = runCmd(upstreamPath, "git", "config", "user.name", "Test User")
	_, _ = runCmd(upstreamPath, "git", "config", "user.email", "test@example.com")
	if _, err := runCmd(upstreamPath, "git", "commit", "-q", "--allow-empty", "-m", "initial"); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		StorageRoot:       filepath.Join(tempDir, "storage"),
		AllowedHosts:      []string{"github.com"},
		UpstreamOverrides: map[string]string{"github.com": upstreamPath},
		RequestWait:       100 * time.Millisecond,
	}
	conv := converter.NewManager(0, 1)
	conv.SetConversionTimeout(10 * time.Second)
	httpServer, err := server.NewHTTPServer(cfg, conv)
	if err != nil {
		t.Fatal(err)
	}
	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/github.com/owner/repo.git/info/refs?service=git-upload-pack", nil)
		rec := httptest.NewRecorder()
		httpServer.ServeHTTP(rec, req)
		return rec
	}

	// The conversion outlasts the request wait (it waits for the only conversion slot)
	if !conv.TryAcquireConversionSlot() {
		t.Fatal("no conversion slot")
	}
	rec := get()
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "retry later") {
		t.Errorf("status %d, Retry-After %q, body %q; want 503 asking to retry", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}

	// It finishes without any client waiting, and the next request is served
	conv.ReleaseConversionSlot()
	conv.WaitIdle()
	if rec := get(); rec.Code != http.StatusOK {
		t.Errorf("after the conversion: status %d, want 200", rec.Code)
	}
}

func TestPushRequestsAreRejectedEarlyWith403(t *testing.T) {
	tempDir := t.TempDir()
	cfg := &config.Config{
		StorageRoot:  filepath.Join(tempDir, "storage"),
		CacheTTL:     0,
		AllowedHosts: []string{"github.com"},
		Shorthands:   config.ParseShorthands(config.DefaultShorthands),
	}

	conv := converter.NewManager(0)
	httpServer, err := server.NewHTTPServer(cfg, conv)
	if err != nil {
		t.Fatal(err)
	}

	pushEndpoints := []struct {
		method string
		url    string
	}{
		{"GET", "/github.com/owner/repo.git/info/refs?service=git-receive-pack"},
		{"POST", "/github.com/owner/repo.git/git-receive-pack"},
	}

	for _, ep := range pushEndpoints {
		t.Run(ep.method+" "+ep.url, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.url, nil)
			rec := httptest.NewRecorder()
			httpServer.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
			}
			if !strings.Contains(rec.Body.String(), "read-only") {
				t.Errorf("body = %q, want read-only explanation", rec.Body.String())
			}

			// Verify that no repository was created on disk
			if _, err := os.Stat(filepath.Join(cfg.StorageRoot, "mirrors", "github.com", "owner", "repo.git")); err == nil {
				t.Errorf("mirror directory was created for rejected push request")
			}
		})
	}
}
