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

// Test upstreams are mirrored like any upstream: each from its own allowed host, which is overridden
// to the upstream's local path (an override applies to a whole host).

// hostOf is the host a test upstream is mirrored from.
func hostOf(upstream string) string {
	return strings.TrimSuffix(filepath.Base(upstream), ".git") + ".test"
}

// urlOf is the URL of a test upstream, e.g. for .gitmodules.
func urlOf(upstream string) string {
	return "https://" + hostOf(upstream) + "/test/" + filepath.Base(upstream)
}

// newSubmoduleManager stores everything under root, so separate managers share nothing, and mirrors
// the given test upstreams.
func newSubmoduleManager(root string, ttl time.Duration, upstreams ...string) *Manager {
	cfg := &config.Config{
		StorageRoot:       root,
		AllowedHosts:      slices.Clone(config.DefaultAllowedHosts),
		UpstreamOverrides: make(map[string]string),
	}
	for _, upstream := range upstreams {
		cfg.AllowedHosts = append(cfg.AllowedHosts, hostOf(upstream))
		cfg.UpstreamOverrides[hostOf(upstream)] = upstream
	}
	m := NewManager(ttl)
	m.SetConfig(cfg)
	return m
}

// repoInfo resolves a test upstream as Evergit resolves its URL.
func repoInfo(t *testing.T, m *Manager, upstream string) *resolver.RepositoryInfo {
	t.Helper()
	info, err := resolver.ParsePath(hostOf(upstream)+"/test/"+filepath.Base(upstream), m.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return info
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
	addGitlink(t, super, "sub", urlOf(sub), gitLine(t, sub, nil, "rev-parse", "HEAD"))
	// Reading the submodule upstream fails ("Could not read from remote repository", as for network
	// errors): unlike a missing repository, not a permanent reason for the synthetic SHA-256
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sub, nil, 0644); err != nil {
		t.Fatal(err)
	}

	m := newSubmoduleManager(t.TempDir(), 0, super, sub)
	info := repoInfo(t, m, super)
	err := m.EnsureRepo(context.Background(), info, nil)
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
	addGitlink(t, super, "sub", urlOf(sub), commit)
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}

	m := newSubmoduleManager(t.TempDir(), 0, super, sub)
	info := repoInfo(t, m, super)
	assertNeedsApproval(t, m.EnsureRepo(context.Background(), info, nil), canonicalRepoID(info), urlOf(sub))

	approveSynthetic(t, m, canonicalRepoID(info)+" "+urlOf(sub))
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
	addGitlink(t, super, "sub", urlOf(sub), missing)

	m := newSubmoduleManager(t.TempDir(), 0, super, sub)
	info := repoInfo(t, m, super)
	assertNeedsApproval(t, m.EnsureRepo(context.Background(), info, nil), canonicalRepoID(info), urlOf(sub))

	approveSynthetic(t, m, "* "+urlOf(sub))
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

	// The submodule lives on a host that is not allowed
	m := newSubmoduleManager(t.TempDir(), 0, super)
	info := repoInfo(t, m, super)
	err := m.EnsureRepo(context.Background(), info, nil)
	assertNeedsApproval(t, err, canonicalRepoID(info), url)
	if !strings.Contains(err.Error(), "-allowed-hosts") {
		t.Errorf("error should mention allowing the host:\n%v", err)
	}

	approveSynthetic(t, m, canonicalRepoID(info)+" "+url)
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
	addGitlink(t, super, "sub", urlOf(sub), gitLine(t, sub, nil, "rev-parse", "HEAD"))

	m := newSubmoduleManager(t.TempDir(), time.Hour, super, sub)
	info := repoInfo(t, m, super)
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}

	// The superproject records a submodule commit newer than the converted submodule
	commitToUpstream(t, sub, "sub 2")
	newCommit := gitLine(t, sub, nil, "rev-parse", "HEAD")
	addGitlink(t, super, "sub", urlOf(sub), newCommit)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(info.ServingPath, "HEAD"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}

	want, err := LookupID(repoInfo(t, m, sub).ServingPath, "sha1", newCommit)
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
	addGitlink(t, mid, "leaf", urlOf(leaf), gitLine(t, leaf, nil, "rev-parse", "HEAD"))
	top := newTestUpstream(t)
	addGitlink(t, top, "mid", urlOf(mid), gitLine(t, mid, nil, "rev-parse", "HEAD"))

	// Requested directly
	direct := newSubmoduleManager(t.TempDir(), 0, mid, leaf)
	midInfo := repoInfo(t, direct, mid)
	if err := direct.EnsureRepo(context.Background(), midInfo, nil); err != nil {
		t.Fatal(err)
	}
	// Reached as a submodule
	nested := newSubmoduleManager(t.TempDir(), 0, top, mid, leaf)
	if err := nested.EnsureRepo(context.Background(), repoInfo(t, nested, top), nil); err != nil {
		t.Fatal(err)
	}

	if got, want := headOf(t, repoInfo(t, nested, mid).ServingPath), headOf(t, midInfo.ServingPath); got != want {
		t.Errorf("submodule reached through a superproject converted to %s, directly to %s", got, want)
	}
}

func TestSubmoduleDiscoveredFromOldHistory(t *testing.T) {
	sub := newTestUpstream(t)
	commit := gitLine(t, sub, nil, "rev-parse", "HEAD")
	super := newTestUpstream(t)
	addGitlink(t, super, "old", urlOf(sub), commit)
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

	m := newSubmoduleManager(t.TempDir(), 0, super, sub)
	info := repoInfo(t, m, super)
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	want, err := LookupID(repoInfo(t, m, sub).ServingPath, "sha1", commit)
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
	addGitlink(t, super, "sub", urlOf(sub), commit)

	m := newSubmoduleManager(t.TempDir(), 0, super, sub)
	info := repoInfo(t, m, super)
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	want, err := LookupID(repoInfo(t, m, sub).ServingPath, "sha1", commit)
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
	addGitlink(t, super, "dir/sub", urlOf(sub), commit)
	away := sub + ".away"
	if err := os.Rename(sub, away); err != nil {
		t.Fatal(err)
	}

	m := newSubmoduleManager(t.TempDir(), 0, super, sub)
	info := repoInfo(t, m, super)
	approveSynthetic(t, m, canonicalRepoID(info)+" "+urlOf(sub))
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
	addGitlink(t, super, "docs", urlOf(super), pages)

	m := newSubmoduleManager(t.TempDir(), 0, super)
	info := repoInfo(t, m, super)
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := gitlinkOf(t, info.ServingPath, "main", "docs"), gitLine(t, info.ServingPath, nil, "--git-dir=.", "rev-parse", "gh-pages"); got != want {
		t.Errorf("gitlink into the repository itself = %s, want its converted gh-pages commit %s", got, want)
	}
}

func TestSubmodulePointingIntoItsOwnRepositoryOnNoBranchIsFetched(t *testing.T) {
	super := newTestUpstream(t)
	m := newSubmoduleManager(t.TempDir(), 0, super)
	info := repoInfo(t, m, super)
	// Mirrored before the commit exists: cloning a local path would copy unreachable objects too
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	// A commit of the repository only a forge ref reaches, like GitHub's refs/pull/*
	gitRaw(t, super, nil, "checkout", "-q", "-b", "pr")
	commitToUpstream(t, super, "pull request")
	commit := gitLine(t, super, nil, "rev-parse", "HEAD")
	gitRaw(t, super, nil, "update-ref", "refs/pull/1/head", commit)
	gitRaw(t, super, nil, "checkout", "-q", "main")
	gitRaw(t, super, nil, "branch", "-D", "pr")
	addGitlink(t, super, "self", urlOf(super), commit)

	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	// The gitlink records the commit as the repository converts it once a branch reaches it
	gitlink := gitlinkOf(t, info.ServingPath, "main", "self")
	gitRaw(t, super, nil, "branch", "pr", commit)
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	if want := gitLine(t, info.ServingPath, nil, "--git-dir=.", "rev-parse", "pr"); gitlink != want {
		t.Errorf("gitlink into the repository itself = %s, want its converted commit %s", gitlink, want)
	}
}

func TestSubmodulePointingIntoItsOwnRepositoryGoneUpstreamNeedsApproval(t *testing.T) {
	super := newTestUpstream(t)
	addGitlink(t, super, "self", urlOf(super), strings.Repeat("ab", 20))

	m := newSubmoduleManager(t.TempDir(), 0, super)
	assertNeedsApproval(t, m.EnsureRepo(context.Background(), repoInfo(t, m, super), nil), canonicalRepoID(repoInfo(t, m, super)), urlOf(super))
}

func TestPartialConversionExtendsWithoutChangingIDs(t *testing.T) {
	up := newTestUpstream(t)
	commitToUpstream(t, up, "second")
	needed := gitLine(t, up, nil, "rev-parse", "HEAD")
	gitRaw(t, up, nil, "checkout", "-q", "-b", "other", "HEAD~1")
	commitToUpstream(t, up, "other branch")
	other := gitLine(t, up, nil, "rev-parse", "HEAD")
	gitRaw(t, up, nil, "checkout", "-q", "main")
	commitToUpstream(t, up, "third")
	later := gitLine(t, up, nil, "rev-parse", "HEAD")

	m := newSubmoduleManager(t.TempDir(), 0, up)
	info := repoInfo(t, m, up)
	ctx := context.Background()
	idOf := func(commit string) string {
		t.Helper()
		id, err := LookupID(info.ServingPath, "sha1", commit)
		if err != nil {
			t.Fatalf("commit %s not converted: %v", commit, err)
		}
		return id
	}
	assertNotConverted := func(commits ...string) {
		t.Helper()
		for _, c := range commits {
			if _, err := LookupID(info.ServingPath, "sha1", c); err == nil {
				t.Errorf("commit %s converted beyond the history of the required commits", c)
			}
		}
	}
	branches := func() string {
		return gitLine(t, info.ServingPath, nil, "--git-dir=.", "for-each-ref", "--format=%(refname)", "refs/heads/")
	}

	// Only the history of the required commit is converted, and only its pinned ref is served
	if err := m.ensureRepo(ctx, info, nil, DefaultMaxSubmoduleDepth, []string{needed}, true); err != nil {
		t.Fatal(err)
	}
	neededID := idOf(needed)
	assertNotConverted(other, later)
	if b := branches(); b != "" {
		t.Errorf("partial conversion serves branches: %s", b)
	}
	build, err := os.Readlink(info.ServingPath)
	if err != nil {
		t.Fatal(err)
	}
	extended := func(step string) {
		t.Helper()
		if got := idOf(needed); got != neededID {
			t.Errorf("%s: SHA-256 of %s changed from %s to %s", step, needed, neededID, got)
		}
		if b, _ := os.Readlink(info.ServingPath); b != build {
			t.Errorf("%s: repository rebuilt instead of extended", step)
		}
	}

	// Another required commit extends it
	if err := m.ensureRepo(ctx, info, nil, DefaultMaxSubmoduleDepth, []string{other}, true); err != nil {
		t.Fatal(err)
	}
	extended("another required commit")
	assertNotConverted(later)

	// A request for the repository itself converts it completely
	if err := m.EnsureRepo(ctx, info, nil); err != nil {
		t.Fatal(err)
	}
	extended("complete conversion")
	if b := branches(); !strings.Contains(b, "refs/heads/main") {
		t.Errorf("complete conversion does not serve the branches: %q", b)
	}

	// Every object has the SHA-256 of a complete conversion from scratch
	fresh := newSubmoduleManager(t.TempDir(), 0, up)
	freshInfo := repoInfo(t, fresh, up)
	if err := fresh.EnsureRepo(ctx, freshInfo, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{needed, other, later} {
		want, err := LookupID(freshInfo.ServingPath, "sha1", c)
		if err != nil {
			t.Fatal(err)
		}
		if got := idOf(c); got != want {
			t.Errorf("commit %s is %s after partial conversions, %s converted completely", c, got, want)
		}
	}

	// A complete conversion is never reduced to a partial one
	if err := m.ensureRepo(ctx, info, nil, DefaultMaxSubmoduleDepth, []string{needed}, true); err != nil {
		t.Fatal(err)
	}
	if b := branches(); !strings.Contains(b, "refs/heads/main") {
		t.Errorf("complete conversion reduced to a partial one: branches %q", b)
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
	addGitlink(t, super, "sub", urlOf(sub), subCommit)
	addGitlink(t, super, "gone", urlOf(gone), goneCommit)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	m := newSubmoduleManager(t.TempDir(), 0, super, sub, gone)
	info := repoInfo(t, m, super)
	approval := canonicalRepoID(info) + " " + urlOf(gone)
	approveSynthetic(t, m, approval)
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
	want, err := LookupID(repoInfo(t, m, sub).ServingPath, "sha1", subCommit)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Submodules[urlOf(sub)].Translated[subCommit]; got != want {
		t.Errorf("state records %s for submodule %s commit %s, want %s", got, sub, subCommit, want)
	}
	synthetic := state.Submodules[urlOf(gone)].Synthetic
	if len(synthetic) != 1 || !slices.Equal(synthetic[0].Commits, []string{goneCommit}) ||
		synthetic[0].ApprovedBy != approval || !strings.Contains(synthetic[0].Reason, "gone upstream") {
		t.Errorf("synthetic decisions of %s = %+v, want commit %s approved by its line, gone upstream", gone, synthetic, goneCommit)
	}
}

func TestSubmoduleURLsNeverReachLocalFiles(t *testing.T) {
	m := newSubmoduleManager(t.TempDir(), 0)
	for _, parent := range []*resolver.RepositoryInfo{
		{Domain: "github.com", Owner: "owner", RepoName: "repo"},
		{Domain: "local", Owner: "test", RepoName: "repo"}, // treated as local paths in the past
	} {
		for _, url := range []string{"/srv/evergit/storage/repos/x.git", "file:///etc/x.git", "file://localhost/srv/x.git", "file:x.git"} {
			if info, err := m.resolveSubmoduleInfo(url, parent); err == nil {
				t.Errorf("submodule %q of %s resolved to %s (%s), want an error", url, canonicalRepoID(parent), canonicalRepoID(info), info.RemoteURL)
			}
		}
	}
}

func TestSubmoduleWithLocalPathIsNotCloned(t *testing.T) {
	// A repository on the server that a public superproject points at
	secret := newTestUpstream(t)
	super := newTestUpstream(t)
	addGitlink(t, super, "secret", secret, gitLine(t, secret, nil, "rev-parse", "HEAD"))

	root := t.TempDir()
	m := newSubmoduleManager(root, 0, super)
	info := repoInfo(t, m, super)
	err := m.EnsureRepo(context.Background(), info, nil)
	assertNeedsApproval(t, err, canonicalRepoID(info), secret)

	// Only the superproject was mirrored
	mirrors, err := filepath.Glob(filepath.Join(root, "mirrors", "*", "*", "*.git"))
	if err != nil {
		t.Fatal(err)
	}
	if len(mirrors) != 1 || mirrors[0] != info.MirrorPath {
		t.Errorf("mirrors = %v, want only the superproject's %s", mirrors, info.MirrorPath)
	}
}

func TestRelativeSubmoduleURLsResolveLikeGit(t *testing.T) {
	m := newSubmoduleManager(t.TempDir(), 0)
	parent := &resolver.RepositoryInfo{Domain: "github.com", Owner: "owner", RepoName: "repo"}
	// As "git submodule init" resolves them against https://github.com/owner/repo.git
	for url, want := range map[string]string{
		"../other.git":       "github.com/owner/other",
		"../../org2/lib.git": "github.com/org2/lib",
		"./nested.git":       "", // https://github.com/owner/repo.git/nested.git is no repository path
	} {
		info, err := m.resolveSubmoduleInfo(url, parent)
		switch {
		case want == "" && err == nil:
			t.Errorf("relative URL %q resolved to %s, want an error", url, canonicalRepoID(info))
		case want != "" && err != nil:
			t.Errorf("relative URL %q: %v, want %s", url, err, want)
		case want != "" && canonicalRepoID(info) != want:
			t.Errorf("relative URL %q resolved to %s, want %s", url, canonicalRepoID(info), want)
		}
	}
}

func TestDiscoverGitlinksQuotedAndSpecialPaths(t *testing.T) {
	super := newTestUpstream(t)
	dummyCommit := strings.Repeat("12", 20)

	testCases := []struct {
		name    string
		path    string
		url     string
		wantSub string
	}{
		{
			name:    "quoted path with embedded double quote and gitmodules",
			path:    `sub"quote`,
			url:     "https://example.com/sub-quote.git",
			wantSub: "https://example.com/sub-quote.git",
		},
		{
			name:    "quoted path with tab and gitmodules",
			path:    "sub\ttab",
			url:     "https://example.com/sub-tab.git",
			wantSub: "https://example.com/sub-tab.git",
		},
		{
			name:    "path with spaces and gitmodules",
			path:    "sub dir",
			url:     "https://example.com/sub-dir.git",
			wantSub: "https://example.com/sub-dir.git",
		},
		{
			name:    "unquoted path with backticks without gitmodules",
			path:    "`backtick`",
			url:     "",
			wantSub: "path:`backtick`",
		},
		{
			name:    "unquoted path with single quotes without gitmodules",
			path:    "'x'",
			url:     "",
			wantSub: "path:'x'",
		},
		{
			name:    "quoted path with embedded double quote without gitmodules",
			path:    `sub"no-modules`,
			url:     "",
			wantSub: `path:sub"no-modules`,
		},
		{
			name:    "quoted path with tab without gitmodules",
			path:    "sub\ttab-no-modules",
			url:     "",
			wantSub: "path:sub\ttab-no-modules",
		},
	}

	for _, tc := range testCases {
		addGitlink(t, super, tc.path, tc.url, dummyCommit)
	}

	targets, err := discoverGitlinks(context.Background(), super, nil, nil, nil)
	if err != nil {
		t.Fatalf("discoverGitlinks: %v", err)
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			commits := targets[tc.wantSub]
			if commits == nil || !commits[dummyCommit] {
				t.Errorf("targets[%q] = %v, want commit %s", tc.wantSub, commits, dummyCommit)
			}
		})
	}
}

func TestDiscoverGitlinksProgress(t *testing.T) {
	super := newTestUpstream(t)
	dummyCommit := strings.Repeat("12", 20)
	addGitlink(t, super, "sub1", "https://example.com/sub1.git", dummyCommit)
	addGitlink(t, super, "sub2", "https://example.com/sub2.git", dummyCommit)

	var lastScanned int
	targets, err := discoverGitlinks(context.Background(), super, nil, nil, func(n int) {
		lastScanned = n
	})
	if err != nil {
		t.Fatalf("discoverGitlinks: %v", err)
	}
	if len(targets) != 2 {
		t.Errorf("targets = %d, want 2", len(targets))
	}
	if lastScanned <= 0 {
		t.Errorf("lastScanned = %d, want > 0", lastScanned)
	}
}

func TestSubmoduleResolutionEmitsProgress(t *testing.T) {
	sub := newTestUpstream(t)
	commit := gitLine(t, sub, nil, "rev-parse", "HEAD")
	super := newTestUpstream(t)
	addGitlink(t, super, "sub", urlOf(sub), commit)

	m := newSubmoduleManager(t.TempDir(), 0, super, sub)
	info := repoInfo(t, m, super)
	var progressBuf strings.Builder
	if err := m.EnsureRepo(context.Background(), info, &progressBuf); err != nil {
		t.Fatal(err)
	}

	out := progressBuf.String()
	for _, expected := range []string{
		"Scanning history for submodules...",
		fmt.Sprintf("Scanning %s: ", info.RepoName),
		"Resolving 1 submodule(s)...",
		"Converting SHA-1 objects to SHA-256...",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("progress output missing %q, got:\n%s", expected, out)
		}
	}
}

func TestSubmoduleWithQuotedPath(t *testing.T) {
	sub := newTestUpstream(t)
	commit := gitLine(t, sub, nil, "rev-parse", "HEAD")
	super := newTestUpstream(t)
	quotedPath := `sub"quote`
	tabPath := "sub\ttab"
	addGitlink(t, super, quotedPath, urlOf(sub), commit)
	addGitlink(t, super, tabPath, urlOf(sub), commit)

	m := newSubmoduleManager(t.TempDir(), 0, super, sub)
	info := repoInfo(t, m, super)
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	want, err := LookupID(repoInfo(t, m, sub).ServingPath, "sha1", commit)
	if err != nil {
		t.Fatalf("submodule was not converted: %v", err)
	}
	if got := gitlinkOf(t, info.ServingPath, "HEAD", quotedPath); got != want {
		t.Errorf("gitlink at %q = %s, want real SHA-256 %s", quotedPath, got, want)
	}
	if got := gitlinkOf(t, info.ServingPath, "HEAD", tabPath); got != want {
		t.Errorf("gitlink at %q = %s, want real SHA-256 %s", tabPath, got, want)
	}
}

