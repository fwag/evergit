package converter

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// planFor syncs the mirror of the requested repository and plans its conversion.
func planFor(t *testing.T, m *Manager, upstream string) (*conversionPlan, error) {
	t.Helper()
	info := repoInfo(t, m, upstream)
	if err := m.syncMirror(context.Background(), info); err != nil {
		t.Fatal(err)
	}
	return m.planConversion(context.Background(), info, nil, nil, DefaultMaxSubmoduleRepos)
}

// sourceIn returns the source the plan decided for a gitlink of a repository.
func sourceIn(t *testing.T, p *conversionPlan, m *Manager, upstream, url, commit string) gitlinkSource {
	t.Helper()
	r := p.items[canonicalRepoID(repoInfo(t, m, upstream))]
	if r == nil {
		t.Fatalf("plan has no item for %s", upstream)
	}
	src, ok := r.sources[url][commit]
	if !ok {
		t.Fatalf("plan decided no source for %s commit %s in %s", url, commit, r.id)
	}
	return src
}

func TestPlanDecidesGitlinkSources(t *testing.T) {
	sub := newTestUpstream(t)
	commit := gitLine(t, sub, nil, "rev-parse", "HEAD")
	super := newTestUpstream(t)
	addGitlink(t, super, "sub", urlOf(sub), commit)

	m := newSubmoduleManager(t.TempDir(), 0, super, sub)
	p, err := planFor(t, m, super)
	if err != nil {
		t.Fatal(err)
	}
	src := sourceIn(t, p, m, super, urlOf(sub), commit)
	if src.repo == nil || src.repo.id != canonicalRepoID(repoInfo(t, m, sub)) {
		t.Errorf("source = %+v, want the submodule's repository", src)
	}
	if len(p.items) != 2 {
		t.Errorf("plan has %d repositories, want 2", len(p.items))
	}
	// Planning converts nothing
	if _, err := LookupID(repoInfo(t, m, sub).ServingPath, "sha1", commit); err == nil {
		t.Error("planning converted the submodule")
	}
}

func TestPlanReportsAllMissingApprovalsAtOnce(t *testing.T) {
	// The superproject and its submodule each record a submodule that cannot be converted
	sub := newTestUpstream(t)
	addGitlink(t, sub, "inner", "https://unusable.invalid/test/inner.git", strings.Repeat("cd", 20))
	super := newTestUpstream(t)
	addGitlink(t, super, "outer", "https://unusable.invalid/test/outer.git", strings.Repeat("ab", 20))
	addGitlink(t, super, "sub", urlOf(sub), gitLine(t, sub, nil, "rev-parse", "HEAD"))

	m := newSubmoduleManager(t.TempDir(), 0, super, sub)
	_, err := planFor(t, m, super)
	if !errors.Is(err, ErrSubmoduleNotApproved) {
		t.Fatalf("plan = %v, want ErrSubmoduleNotApproved", err)
	}
	for _, line := range []string{
		canonicalRepoID(repoInfo(t, m, super)) + " https://unusable.invalid/test/outer.git",
		canonicalRepoID(repoInfo(t, m, sub)) + " https://unusable.invalid/test/inner.git",
	} {
		if !strings.Contains(err.Error(), line) {
			t.Errorf("error should list the approval %q:\n%v", line, err)
		}
	}
}

func TestPlanCycleWithinRecordedHistory(t *testing.T) {
	repoA := newTestUpstream(t)
	repoB := newTestUpstream(t)
	a := gitLine(t, repoA, nil, "rev-parse", "HEAD")
	addGitlink(t, repoB, "subA", urlOf(repoA), a)
	b := gitLine(t, repoB, nil, "rev-parse", "HEAD")
	addGitlink(t, repoA, "subB", urlOf(repoB), b)

	m := newSubmoduleManager(t.TempDir(), 0, repoA, repoB)
	p, err := planFor(t, m, repoA)
	if err != nil {
		t.Fatal(err)
	}
	if src := sourceIn(t, p, m, repoA, urlOf(repoB), b); src.repo == nil || src.repo.id != canonicalRepoID(repoInfo(t, m, repoB)) {
		t.Errorf("A's gitlink to B: source %+v, want B", src)
	}
	if src := sourceIn(t, p, m, repoB, urlOf(repoA), a); src.repo != p.top {
		t.Errorf("B's gitlink to A: source %+v, want A", src)
	}
}

func TestPlanMovedSubmoduleTranslatedFromNewURL(t *testing.T) {
	leaf := newTestUpstream(t)
	commit := gitLine(t, leaf, nil, "rev-parse", "HEAD")
	const gone = "https://unusable.invalid/test/leaf.git"
	super := newTestUpstream(t)
	addGitlink(t, super, "lib", gone, commit)
	gitRaw(t, super, nil, "config", "-f", ".gitmodules", "submodule.lib.url", urlOf(leaf))
	gitRaw(t, super, nil, "add", ".gitmodules")
	gitRaw(t, super, nil, "commit", "-q", "-m", "move lib")

	m := newSubmoduleManager(t.TempDir(), 0, super, leaf)
	p, err := planFor(t, m, super)
	if err != nil {
		t.Fatal(err)
	}
	if src := sourceIn(t, p, m, super, gone, commit); src.repo == nil || src.repo.id != canonicalRepoID(repoInfo(t, m, leaf)) {
		t.Errorf("source = %+v, want the repository the path moved to", src)
	}
}

func TestPlanOwnCommitOnNoBranchIsFetched(t *testing.T) {
	super := newTestUpstream(t)
	m := newSubmoduleManager(t.TempDir(), 0, super)
	// Mirrored before the commit exists: cloning a local path would copy unreachable objects too
	if err := m.syncMirror(context.Background(), repoInfo(t, m, super)); err != nil {
		t.Fatal(err)
	}
	gitRaw(t, super, nil, "checkout", "-q", "-b", "pr")
	commitToUpstream(t, super, "pull request")
	commit := gitLine(t, super, nil, "rev-parse", "HEAD")
	gitRaw(t, super, nil, "update-ref", "refs/pull/1/head", commit)
	gitRaw(t, super, nil, "checkout", "-q", "main")
	gitRaw(t, super, nil, "branch", "-D", "pr")
	addGitlink(t, super, "self", urlOf(super), commit)

	p, err := planFor(t, m, super)
	if err != nil {
		t.Fatal(err)
	}
	if src := sourceIn(t, p, m, super, urlOf(super), commit); src.repo != p.top {
		t.Errorf("source = %+v, want the repository itself, translating the fetched commit", src)
	}
}

func TestPlanRepositoryLimit(t *testing.T) {
	sub := newTestUpstream(t)
	super := newTestUpstream(t)
	addGitlink(t, super, "sub", urlOf(sub), gitLine(t, sub, nil, "rev-parse", "HEAD"))

	m := newSubmoduleManager(t.TempDir(), 0, super, sub)
	info := repoInfo(t, m, super)
	if err := m.syncMirror(context.Background(), info); err != nil {
		t.Fatal(err)
	}
	if _, err := m.planConversion(context.Background(), info, nil, nil, 0); !errors.Is(err, ErrTooManySubmodules) {
		t.Fatalf("plan allowing no submodule repository = %v, want ErrTooManySubmodules", err)
	}
	// The requested repository does not count
	if _, err := m.planConversion(context.Background(), info, nil, nil, 1); err != nil {
		t.Fatalf("plan allowing the one submodule repository: %v", err)
	}
}
