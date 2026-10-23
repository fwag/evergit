package server_test

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"evergit/internal/config"
	"evergit/internal/converter"
	"evergit/internal/server"
)

// TestIncrementalFetchThroughEvergit clones through Evergit, lets upstream move on, and fetches:
// the client must receive exactly git's own SHA-256 translation of the new history, while the
// served repository is extended in place and everything fetched before keeps its ids.
func TestIncrementalFetchThroughEvergit(t *testing.T) {
	tempDir := t.TempDir()

	// Git records the SHA-256 translation of every object it writes in this upstream
	upstream := filepath.Join(tempDir, "upstream")
	mustRun(t, "", "git", "init", "-q", "--initial-branch=main", "--object-format=sha1", upstream)
	mustRun(t, upstream, "git", "config", "core.repositoryformatversion", "1")
	mustRun(t, upstream, "git", "config", "extensions.compatObjectFormat", "sha256")
	mustRun(t, upstream, "git", "config", "user.name", "Test User")
	mustRun(t, upstream, "git", "config", "user.email", "test@example.com")
	if err := exec.Command("git", "-C", upstream, "commit", "-q", "--allow-empty", "-m", "initial").Run(); err != nil {
		t.Skipf("git does not support extensions.compatObjectFormat: %v", err)
	}
	commit := func(path, content, msg string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(upstream, path)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(upstream, path), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		mustRun(t, upstream, "git", "add", path)
		mustRun(t, upstream, "git", "commit", "-q", "-m", msg)
	}
	commit("dir/a.txt", "a", "add a")
	mustRun(t, upstream, "git", "tag", "-a", "v1", "-m", "v1")
	mustRun(t, upstream, "git", "checkout", "-q", "-b", "feature")
	commit("feature.txt", "f", "feature work")
	mustRun(t, upstream, "git", "checkout", "-q", "main")
	mustRun(t, upstream, "git", "branch", "obsolete")

	cfg := &config.Config{
		StorageRoot:       filepath.Join(tempDir, "storage"),
		AllowedHosts:      []string{"local.test"},
		UpstreamOverrides: map[string]string{"local.test": upstream},
	}
	conv := converter.NewManager(0) // every fetch checks upstream
	conv.SetConfig(cfg)
	httpServer, err := server.NewHTTPServer(cfg, conv)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(httpServer)
	defer ts.Close()

	client := filepath.Join(tempDir, "client")
	mustRun(t, "", "git", "clone", "-q", ts.URL+"/local.test/test/repo.git", client)
	assertMatchesUpstream(t, upstream, client)
	before := mustRun(t, client, "git", "rev-parse", "refs/remotes/origin/main")
	serving := filepath.Join(cfg.StorageRoot, "repos", "local.test", "test", "repo.git")
	build, err := filepath.EvalSymlinks(serving)
	if err != nil {
		t.Fatal(err)
	}

	// Upstream moves on: new commits in an existing directory, a merge, a new tag and branch, and a
	// deleted branch
	commit("dir/b.txt", "b", "add b")
	mustRun(t, upstream, "git", "merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	mustRun(t, upstream, "git", "tag", "-a", "v2", "-m", "v2")
	mustRun(t, upstream, "git", "branch", "release", "v1")
	mustRun(t, upstream, "git", "branch", "-D", "obsolete")

	mustRun(t, client, "git", "fetch", "-q", "--prune", "--tags", "origin")
	assertMatchesUpstream(t, upstream, client)

	if rebuilt, err := filepath.EvalSymlinks(serving); err != nil || rebuilt != build {
		t.Errorf("served repository was rebuilt (%s -> %s, %v) instead of extended", build, rebuilt, err)
	}
	if err := exec.Command("git", "-C", client, "merge-base", "--is-ancestor", before, "refs/remotes/origin/main").Run(); err != nil {
		t.Errorf("previously fetched main %s is no longer an ancestor of the fetched main: %v", before, err)
	}
	if ids := mustRun(t, client, "git", "for-each-ref", "refs/remotes/origin/obsolete"); ids != "" {
		t.Errorf("branch deleted upstream is still fetched: %s", ids)
	}
	mustRun(t, client, "git", "fsck", "--strict", "--no-dangling")

	// Fetching again without upstream changes is a no-op
	mustRun(t, client, "git", "fetch", "-q", "--prune", "--tags", "origin")
	assertMatchesUpstream(t, upstream, client)
}

// assertMatchesUpstream checks that the client's remote branches and tags are git's own SHA-256
// translation of the upstream's branches and tags.
func assertMatchesUpstream(t *testing.T, upstream, client string) {
	t.Helper()
	var want, got []string
	for _, ref := range strings.Fields(mustRun(t, upstream, "git", "for-each-ref", "--format=%(refname)", "refs/heads", "refs/tags")) {
		id := mustRun(t, upstream, "git", "rev-parse", "--output-object-format=sha256", ref)
		want = append(want, id+" "+strings.Replace(ref, "refs/heads/", "refs/remotes/origin/", 1))
	}
	for _, line := range strings.Split(mustRun(t, client, "git", "for-each-ref", "--sort=refname", "--format=%(objectname) %(refname)", "refs/remotes/origin", "refs/tags"), "\n") {
		if !strings.HasSuffix(line, " refs/remotes/origin/HEAD") {
			got = append(got, line)
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if g, w := strings.Join(got, "\n"), strings.Join(want, "\n"); g != w {
		t.Errorf("client refs differ from git's own translation of upstream\ngot:\n%s\nwant:\n%s", g, w)
	}
}

func mustRun(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	out, err := runCmd(dir, name, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCloneThroughEquivalentURLs(t *testing.T) {
	tempDir := t.TempDir()
	upstream := filepath.Join(tempDir, "upstream")
	mustRun(t, "", "git", "init", "-q", "--initial-branch=main", "--object-format=sha1", upstream)
	mustRun(t, upstream, "git", "-c", "user.name=T", "-c", "user.email=t@e", "commit", "-q", "--allow-empty", "-m", "initial")

	cfg := &config.Config{
		StorageRoot:       filepath.Join(tempDir, "storage"),
		AllowedHosts:      []string{"github.com"},
		Shorthands:        config.ParseShorthands(config.DefaultShorthands),
		UpstreamOverrides: map[string]string{"github.com": upstream},
	}
	conv := converter.NewManager(0)
	conv.SetConfig(cfg)
	httpServer, err := server.NewHTTPServer(cfg, conv)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(httpServer)
	defer ts.Close()

	// All name the same repository: a shorthand, no ".git", and another case on a case-insensitive forge
	var heads []string
	for i, path := range []string{"/github.com/owner/repo.git", "/github/owner/repo.git", "/github.com/owner/repo", "/github/Owner/Repo"} {
		clone := filepath.Join(tempDir, "clone"+string(rune('0'+i)))
		mustRun(t, "", "git", "clone", "-q", ts.URL+path, clone)
		heads = append(heads, mustRun(t, clone, "git", "rev-parse", "HEAD"))
	}
	for _, head := range heads[1:] {
		if head != heads[0] {
			t.Errorf("clones through equivalent URLs differ: %v", heads)
		}
	}
}
