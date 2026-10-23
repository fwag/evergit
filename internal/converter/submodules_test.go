package converter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"evergit/internal/config"
	"evergit/internal/resolver"
)

// newSubmoduleManager stores local submodules under root, so separate managers share nothing.
func newSubmoduleManager(root string, ttl time.Duration) *Manager {
	m := NewManager(ttl)
	m.SetConfig(&config.Config{StorageRoot: root, AllowedHosts: config.DefaultAllowedHosts})
	return m
}

func repoInfoAt(root, upstream, name string) *resolver.RepositoryInfo {
	return &resolver.RepositoryInfo{
		Domain:      "local",
		Owner:       "test",
		RepoName:    name,
		RemoteURL:   upstream,
		MirrorPath:  filepath.Join(root, "mirrors", "local", "test", name+".git"),
		ServingPath: filepath.Join(root, "repos", "local", "test", name+".git"),
		BuildsPath:  filepath.Join(root, "builds", "local", "test", name+".git"),
	}
}

// submoduleServingPath is where a local submodule is converted under root.
func submoduleServingPath(root, upstream string) string {
	return filepath.Join(root, "repos", "local", "submodule", filepath.Base(upstream))
}

// addGitlink records commit of sub at path in super, with a .gitmodules entry unless url is "".
func addGitlink(t *testing.T, super, path, url, commit string) {
	t.Helper()
	if url != "" {
		gitRaw(t, super, nil, "config", "-f", ".gitmodules", "submodule."+path+".path", path)
		gitRaw(t, super, nil, "config", "-f", ".gitmodules", "submodule."+path+".url", url)
		gitRaw(t, super, nil, "add", ".gitmodules")
	}
	gitRaw(t, super, nil, "update-index", "--add", "--cacheinfo", "160000,"+commit+","+path)
	gitRaw(t, super, nil, "commit", "-q", "-m", "record "+path+" at "+commit)
}

// approveSynthetic configures m to read an approvals file with lines, as the administrator would.
func approveSynthetic(t *testing.T, m *Manager, lines ...string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synthetic-gitlinks")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if m.cfg == nil {
		m.SetConfig(&config.Config{})
	}
	m.cfg.SyntheticGitlinksFile = path
}

// assertNeedsApproval checks that err asks for approval of submodule, suggesting the approval line.
func assertNeedsApproval(t *testing.T, err error, superproject, submodule string) {
	t.Helper()
	if !errors.Is(err, ErrSubmoduleNotApproved) {
		t.Fatalf("EnsureRepo = %v, want ErrSubmoduleNotApproved", err)
	}
	if line := superproject + " " + submodule; !strings.Contains(err.Error(), line) {
		t.Errorf("error should suggest the approval line %q:\n%v", line, err)
	}
}

func headOf(t *testing.T, repoPath string) string {
	t.Helper()
	return gitLine(t, repoPath, nil, "--git-dir=.", "rev-parse", "HEAD")
}

func gitlinkOf(t *testing.T, repoPath, rev, path string) string {
	t.Helper()
	return gitLine(t, repoPath, nil, "--git-dir=.", "rev-parse", rev+":"+path)
}

func TestSubmoduleTemporaryFailureFailsConversion(t *testing.T) {
	sub := newTestUpstream(t)
	super := newTestUpstream(t)
	addGitlink(t, super, "sub", sub, gitLine(t, sub, nil, "rev-parse", "HEAD"))
	// Reading the submodule upstream fails ("Could not read from remote repository", as for network
	// errors): unlike a missing repository, not a permanent reason for the synthetic SHA-256
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sub, nil, 0644); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	info := repoInfoAt(root, super, "super")
	err := newSubmoduleManager(root, 0).EnsureRepo(context.Background(), info, nil)
	if !errors.Is(err, ErrSubmoduleUnavailable) {
		t.Fatalf("EnsureRepo = %v, want ErrSubmoduleUnavailable", err)
	}
	if _, statErr := os.Stat(info.ServingPath); statErr == nil {
		t.Error("superproject was published although its submodule failed")
	}
}

func TestSubmoduleGoneUpstreamNeedsApproval(t *testing.T) {
	sub := newTestUpstream(t)
	commit := gitLine(t, sub, nil, "rev-parse", "HEAD")
	super := newTestUpstream(t)
	addGitlink(t, super, "sub", sub, commit)
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	info := repoInfoAt(root, super, "super")
	m := newSubmoduleManager(root, 0)
	assertNeedsApproval(t, m.EnsureRepo(context.Background(), info, nil), "local/test/super", sub)

	approveSynthetic(t, m, "local/test/super "+sub)
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := gitlinkOf(t, info.ServingPath, "HEAD", "sub"), commit+syntheticSHA256Padding; got != want {
		t.Errorf("approved gitlink to a deleted repository = %s, want the synthetic %s", got, want)
	}
}

func TestSubmoduleCommitMissingUpstreamNeedsApproval(t *testing.T) {
	sub := newTestUpstream(t)
	super := newTestUpstream(t)
	missing := strings.Repeat("ab", 20)
	addGitlink(t, super, "sub", sub, missing)

	root := t.TempDir()
	info := repoInfoAt(root, super, "super")
	m := newSubmoduleManager(root, 0)
	assertNeedsApproval(t, m.EnsureRepo(context.Background(), info, nil), "local/test/super", sub)

	approveSynthetic(t, m, "* "+sub)
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := gitlinkOf(t, info.ServingPath, "HEAD", "sub"), missing+syntheticSHA256Padding; got != want {
		t.Errorf("approved gitlink to a commit the submodule lacks = %s, want the synthetic %s", got, want)
	}
}

func TestSubmoduleHostNotAllowedNeedsApproval(t *testing.T) {
	super := newTestUpstream(t)
	url := "https://git.example.org/team/lib.git"
	commit := strings.Repeat("cd", 20)
	addGitlink(t, super, "lib", url, commit)

	// A superproject on an allowed forge, whose submodule lives on a host that is not allowed
	root := t.TempDir()
	info := repoInfoAt(root, super, "super")
	info.Domain, info.Owner = "github.com", "owner"
	m := newSubmoduleManager(root, 0)
	err := m.EnsureRepo(context.Background(), info, nil)
	assertNeedsApproval(t, err, "github.com/owner/super", url)
	if !strings.Contains(err.Error(), "-allowed-hosts") {
		t.Errorf("error should mention allowing the host:\n%v", err)
	}

	approveSynthetic(t, m, "github.com/owner/super "+url)
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := gitlinkOf(t, info.ServingPath, "HEAD", "lib"), commit+syntheticSHA256Padding; got != want {
		t.Errorf("approved gitlink to a host that is not allowed = %s, want the synthetic %s", got, want)
	}
}

func TestSubmoduleWithinTTLIsRefreshed(t *testing.T) {
	sub := newTestUpstream(t)
	super := newTestUpstream(t)
	addGitlink(t, super, "sub", sub, gitLine(t, sub, nil, "rev-parse", "HEAD"))

	root := t.TempDir()
	info := repoInfoAt(root, super, "super")
	m := newSubmoduleManager(root, time.Hour)
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}

	// The superproject records a submodule commit newer than the converted submodule
	commitToUpstream(t, sub, "sub 2")
	newCommit := gitLine(t, sub, nil, "rev-parse", "HEAD")
	addGitlink(t, super, "sub", sub, newCommit)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(info.ServingPath, "HEAD"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}

	want, err := LookupID(submoduleServingPath(root, sub), "sha1", newCommit)
	if err != nil {
		t.Fatalf("submodule was not refreshed: %v", err)
	}
	if got := gitlinkOf(t, info.ServingPath, "HEAD", "sub"); got != want {
		t.Errorf("gitlink = %s, want the submodule's real SHA-256 %s", got, want)
	}
}

func TestSubmoduleConversionIndependentOfRequestOrder(t *testing.T) {
	leaf := newTestUpstream(t)
	mid := newTestUpstream(t)
	addGitlink(t, mid, "leaf", leaf, gitLine(t, leaf, nil, "rev-parse", "HEAD"))
	top := newTestUpstream(t)
	addGitlink(t, top, "mid", mid, gitLine(t, mid, nil, "rev-parse", "HEAD"))

	// Requested directly
	direct := t.TempDir()
	midInfo := repoInfoAt(direct, mid, "mid")
	if err := newSubmoduleManager(direct, 0).EnsureRepo(context.Background(), midInfo, nil); err != nil {
		t.Fatal(err)
	}
	// Reached as a submodule
	nested := t.TempDir()
	if err := newSubmoduleManager(nested, 0).EnsureRepo(context.Background(), repoInfoAt(nested, top, "top"), nil); err != nil {
		t.Fatal(err)
	}

	if got, want := headOf(t, submoduleServingPath(nested, mid)), headOf(t, midInfo.ServingPath); got != want {
		t.Errorf("submodule reached through a superproject converted to %s, directly to %s", got, want)
	}
}

func TestSubmoduleDiscoveredFromOldHistory(t *testing.T) {
	sub := newTestUpstream(t)
	commit := gitLine(t, sub, nil, "rev-parse", "HEAD")
	super := newTestUpstream(t)
	addGitlink(t, super, "old", sub, commit)
	recording := gitLine(t, super, nil, "rev-parse", "HEAD")
	// The submodule is removed, and .gitmodules changes many times afterwards
	gitRaw(t, super, nil, "rm", "-q", "--cached", "old")
	gitRaw(t, super, nil, "config", "-f", ".gitmodules", "--remove-section", "submodule.old")
	gitRaw(t, super, nil, "add", ".gitmodules")
	gitRaw(t, super, nil, "commit", "-q", "-m", "remove submodule")
	for i := 0; i < 55; i++ {
		gitRaw(t, super, nil, "config", "-f", ".gitmodules", "submodule.dummy.url", fmt.Sprintf("https://example.invalid/%d.git", i))
		gitRaw(t, super, nil, "add", ".gitmodules")
		gitRaw(t, super, nil, "commit", "-q", "-m", fmt.Sprintf("dummy %d", i))
	}

	root := t.TempDir()
	info := repoInfoAt(root, super, "super")
	if err := newSubmoduleManager(root, 0).EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	want, err := LookupID(submoduleServingPath(root, sub), "sha1", commit)
	if err != nil {
		t.Fatalf("submodule only in old history was not converted: %v", err)
	}
	convertedRecording, err := LookupID(info.ServingPath, "sha1", recording)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitlinkOf(t, info.ServingPath, convertedRecording, "old"); got != want {
		t.Errorf("gitlink in old history = %s, want the submodule's real SHA-256 %s", got, want)
	}
}

func TestSubmoduleCommitOnNoBranchIsFetched(t *testing.T) {
	sub := newTestUpstream(t)
	// A commit only a forge ref reaches, like GitHub's refs/pull/*
	gitRaw(t, sub, nil, "checkout", "-q", "-b", "pr")
	commitToUpstream(t, sub, "pull request")
	commit := gitLine(t, sub, nil, "rev-parse", "HEAD")
	gitRaw(t, sub, nil, "update-ref", "refs/pull/1/head", commit)
	gitRaw(t, sub, nil, "checkout", "-q", "main")
	gitRaw(t, sub, nil, "branch", "-D", "pr")
	super := newTestUpstream(t)
	addGitlink(t, super, "sub", sub, commit)

	root := t.TempDir()
	info := repoInfoAt(root, super, "super")
	if err := newSubmoduleManager(root, 0).EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	want, err := LookupID(submoduleServingPath(root, sub), "sha1", commit)
	if err != nil {
		t.Fatalf("commit on no branch was not fetched into the submodule: %v", err)
	}
	if got := gitlinkOf(t, info.ServingPath, "HEAD", "sub"); got != want {
		t.Errorf("gitlink = %s, want the submodule's real SHA-256 %s", got, want)
	}
}

func TestGitlinkDecisionsAreKept(t *testing.T) {
	sub := newTestUpstream(t)
	commit := gitLine(t, sub, nil, "rev-parse", "HEAD")
	super := newTestUpstream(t)
	addGitlink(t, super, "dir/sub", sub, commit)
	away := sub + ".away"
	if err := os.Rename(sub, away); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	info := repoInfoAt(root, super, "super")
	m := newSubmoduleManager(root, 0)
	approveSynthetic(t, m, "local/test/super "+sub)
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	padded := commit + syntheticSHA256Padding

	// The approval is withdrawn and the submodule is back, but a commit that changes the gitlink's
	// directory must not translate the same submodule commit differently
	approveSynthetic(t, m)
	if err := os.Rename(away, sub); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(super, "dir"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(super, "dir", "file"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRaw(t, super, nil, "add", "dir/file")
	gitRaw(t, super, nil, "commit", "-q", "-m", "touch dir")
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	if got := gitlinkOf(t, info.ServingPath, "HEAD", "dir/sub"); got != padded {
		t.Errorf("incremental conversion translated the gitlink to %s, earlier to %s", got, padded)
	}

	// Nor must a rebuild, e.g. for new conversion rules: synthetic decisions do not depend on them
	build := gitLine(t, "", nil, "-C", info.ServingPath, "rev-parse", "--absolute-git-dir")
	setConversionVersion(t, info.ServingPath, conversionVersion-1)
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	if rebuilt := gitLine(t, "", nil, "-C", info.ServingPath, "rev-parse", "--absolute-git-dir"); rebuilt == build {
		t.Fatal("repository converted with other rules was not rebuilt")
	}
	if got := gitlinkOf(t, info.ServingPath, "HEAD", "dir/sub"); got != padded {
		t.Errorf("rebuild translated the gitlink to %s, earlier to %s", got, padded)
	}
}

// setConversionVersion rewrites a serving repository's state as if converted with other rules.
func setConversionVersion(t *testing.T, servingPath string, version int) {
	t.Helper()
	state, err := loadState(servingPath)
	if err != nil {
		t.Fatal(err)
	}
	state.ConversionVersion = version
	if err := state.save(servingPath); err != nil {
		t.Fatal(err)
	}
}

func TestSubmodulePointingIntoItsOwnRepository(t *testing.T) {
	super := newTestUpstream(t)
	// Like a docs submodule tracking the repository's own gh-pages branch
	gitRaw(t, super, nil, "checkout", "-q", "--orphan", "gh-pages")
	gitRaw(t, super, nil, "rm", "-q", "-rf", ".")
	commitToUpstream(t, super, "pages")
	pages := gitLine(t, super, nil, "rev-parse", "HEAD")
	gitRaw(t, super, nil, "checkout", "-q", "main")
	addGitlink(t, super, "docs", super, pages)

	root := t.TempDir()
	info := repoInfoAt(root, super, "super")
	if err := newSubmoduleManager(root, 0).EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := gitlinkOf(t, info.ServingPath, "main", "docs"), gitLine(t, info.ServingPath, nil, "--git-dir=.", "rev-parse", "gh-pages"); got != want {
		t.Errorf("gitlink into the repository itself = %s, want its converted gh-pages commit %s", got, want)
	}
}

func TestMarkGoneClassifiesUpstreamErrors(t *testing.T) {
	cases := []struct {
		stderr string
		gone   bool
	}{
		// Messages captured from git 2.55
		{"remote: Repository not found.\nfatal: Authentication failed for 'https://github.com/git/missing/'", true},
		{"remote: HTTP Basic: Access denied. ...\nfatal: Authentication failed for 'https://gitlab.com/libvirt/missing.git/'", true},
		{"fatal: repository '/srv/missing' does not exist", true},
		{"fatal: remote error: upload-pack: not our ref abababababababababababababababababababab", true},
		{"fatal: unable to access 'https://example.org/x/y/': Could not resolve host: example.org", false},
		{"fatal: invalid gitfile format: /srv/file\nfatal: Could not read from remote repository.", false},
		{"fatal: unable to access 'https://github.com/x/y/': The requested URL returned error: 503", false},
	}
	for _, tc := range cases {
		if gone := errors.Is(markGone(errors.New(tc.stderr)), errUpstreamGone); gone != tc.gone {
			t.Errorf("markGone(%q) gone = %v, want %v", tc.stderr, gone, tc.gone)
		}
	}
}

func TestStateRecordsSubmoduleDecisions(t *testing.T) {
	sub := newTestUpstream(t)
	subCommit := gitLine(t, sub, nil, "rev-parse", "HEAD")
	gone := newTestUpstream(t)
	commitToUpstream(t, gone, "only in the gone repository") // not the same commit as sub's
	goneCommit := gitLine(t, gone, nil, "rev-parse", "HEAD")
	super := newTestUpstream(t)
	addGitlink(t, super, "sub", sub, subCommit)
	addGitlink(t, super, "gone", gone, goneCommit)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	info := repoInfoAt(root, super, "super")
	m := newSubmoduleManager(root, 0)
	approveSynthetic(t, m, "local/test/super "+gone)
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}

	state, err := loadState(info.ServingPath)
	if err != nil {
		t.Fatal(err)
	}
	if state.ConversionVersion != conversionVersion {
		t.Errorf("conversion_version = %d, want %d", state.ConversionVersion, conversionVersion)
	}
	want, err := LookupID(submoduleServingPath(root, sub), "sha1", subCommit)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Submodules[sub].Translated[subCommit]; got != want {
		t.Errorf("state records %s for submodule %s commit %s, want %s", got, sub, subCommit, want)
	}
	synthetic := state.Submodules[gone].Synthetic
	if len(synthetic) != 1 || !slices.Equal(synthetic[0].Commits, []string{goneCommit}) ||
		synthetic[0].ApprovedBy != "local/test/super "+gone || !strings.Contains(synthetic[0].Reason, "gone upstream") {
		t.Errorf("synthetic decisions of %s = %+v, want commit %s approved by its line, gone upstream", gone, synthetic, goneCommit)
	}
}
