package converter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"evergit/internal/resolver"
)

func TestConverter(t *testing.T) {
	// Create a temp workspace
	tempDir, err := os.MkdirTemp("", "evergit-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// 1. Create a dummy SHA1 upstream repository
	upstreamPath := filepath.Join(tempDir, "upstream.git")
	err = os.MkdirAll(upstreamPath, 0755)
	if err != nil {
		t.Fatalf("failed to create upstream dir: %v", err)
	}

	_, err = runCmd(upstreamPath, "git", "init", "--initial-branch=main", "--object-format=sha1")
	if err != nil {
		// Try without --initial-branch in case of older Git version
		_, err = runCmd(upstreamPath, "git", "init", "--object-format=sha1")
		if err != nil {
			t.Fatalf("failed to init upstream SHA1 repo: %v", err)
		}
	}

	// Configure git user for the test commits to avoid system configuration dependencies
	_, _ = runCmd(upstreamPath, "git", "config", "user.name", "Test User")
	_, _ = runCmd(upstreamPath, "git", "config", "user.email", "test@example.com")

	// Create and commit a file
	testFile := filepath.Join(upstreamPath, "test.txt")
	err = os.WriteFile(testFile, []byte("hello world"), 0644)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	_, err = runCmd(upstreamPath, "git", "add", "test.txt")
	if err != nil {
		t.Fatalf("failed to add file to git: %v", err)
	}

	_, err = runCmd(upstreamPath, "git", "commit", "-m", "initial commit")
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	// 2. Setup the Converter Manager
	storageRoot := filepath.Join(tempDir, "storage")
	manager := NewManager(1 * time.Second)

	info := &resolver.RepositoryInfo{
		Domain:      "local",
		Owner:       "test",
		RepoName:    "repo",
		RemoteURL:   upstreamPath, // Git clones directly from local directory
		MirrorPath:  filepath.Join(storageRoot, "mirrors", "local", "test", "repo.git"),
		ServingPath: filepath.Join(storageRoot, "repos", "local", "test", "repo.git"),
		BuildsPath:  filepath.Join(storageRoot, "builds", "local", "test", "repo.git"),
	}

	// 3. Ensure the repository converts successfully
	err = manager.EnsureRepo(context.Background(), info, nil)
	if err != nil {
		t.Fatalf("EnsureRepo failed: %v", err)
	}

	// Verify the final repository exists and is SHA256
	format, err := runCmd(info.ServingPath, "git", "rev-parse", "--show-object-format")
	if err != nil {
		t.Fatalf("failed to check format of converted repo: %v", err)
	}

	if format != "sha256" {
		t.Errorf("expected format sha256, got %q", format)
	}

	// Verify we can see our commit in the converted repo
	logOut, err := runCmd(info.ServingPath, "git", "log", "--oneline")
	if err != nil {
		t.Fatalf("failed to run git log in converted repo: %v", err)
	}

	if !strings.Contains(logOut, "initial commit") {
		t.Errorf("expected log to contain 'initial commit', got %q", logOut)
	}
}

func TestConverterForcePushBackup(t *testing.T) {
	// Create a temp workspace
	tempDir, err := os.MkdirTemp("", "evergit-test-backup-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// 1. Create a dummy SHA1 upstream repository
	upstreamPath := filepath.Join(tempDir, "upstream.git")
	err = os.MkdirAll(upstreamPath, 0755)
	if err != nil {
		t.Fatalf("failed to create upstream dir: %v", err)
	}

	_, err = runCmd(upstreamPath, "git", "init", "--initial-branch=main", "--object-format=sha1")
	if err != nil {
		_, err = runCmd(upstreamPath, "git", "init", "--object-format=sha1")
		if err != nil {
			t.Fatalf("failed to init upstream: %v", err)
		}
	}

	// Configure git user
	_, _ = runCmd(upstreamPath, "git", "config", "user.name", "Test User")
	_, _ = runCmd(upstreamPath, "git", "config", "user.email", "test@example.com")

	// Create and commit a file (Commit A)
	testFile := filepath.Join(upstreamPath, "test.txt")
	_ = os.WriteFile(testFile, []byte("hello commit A"), 0644)
	_, _ = runCmd(upstreamPath, "git", "add", "test.txt")
	_, _ = runCmd(upstreamPath, "git", "commit", "-m", "commit A")

	// Read old HEAD hash
	oldHeadSHA1, _ := runCmd(upstreamPath, "git", "rev-parse", "HEAD")

	// 2. Setup the Converter Manager
	storageRoot := filepath.Join(tempDir, "storage")
	manager := NewManager(0) // Zero TTL syncs on every call

	info := &resolver.RepositoryInfo{
		Domain:      "local",
		Owner:       "test",
		RepoName:    "repo-backup",
		RemoteURL:   upstreamPath,
		MirrorPath:  filepath.Join(storageRoot, "mirrors", "local", "test", "repo-backup.git"),
		ServingPath: filepath.Join(storageRoot, "repos", "local", "test", "repo-backup.git"),
		BuildsPath:  filepath.Join(storageRoot, "builds", "local", "test", "repo-backup.git"),
	}

	// First JIT conversion (Initial sync)
	err = manager.EnsureRepo(context.Background(), info, nil)
	if err != nil {
		t.Fatalf("First EnsureRepo failed: %v", err)
	}

	// 3. Force push / Reset on upstream (Commit B replacing Commit A)
	_ = os.WriteFile(testFile, []byte("hello commit B (force-pushed)"), 0644)
	_, _ = runCmd(upstreamPath, "git", "add", "test.txt")
	_, _ = runCmd(upstreamPath, "git", "commit", "--amend", "-m", "commit B (amended)")

	newHeadSHA1, _ := runCmd(upstreamPath, "git", "rev-parse", "HEAD")
	if oldHeadSHA1 == newHeadSHA1 {
		t.Fatalf("Amended commit should have a different hash than the original!")
	}

	// Second JIT conversion (Triggers fetch and backup check!)
	err = manager.EnsureRepo(context.Background(), info, nil)
	if err != nil {
		t.Fatalf("Second EnsureRepo failed: %v", err)
	}

	// 4. Verify that a backup reference was created in the mirror pointing to the old SHA1 commit!
	backupRefsOutput, err := runCmd(info.MirrorPath, "git", "show-ref")
	if err != nil {
		t.Fatalf("failed to show-ref in mirror: %v", err)
	}

	if !strings.Contains(backupRefsOutput, "refs/evergit-backups/heads/") {
		t.Errorf("Expected backup ref to be created under refs/evergit-backups/heads/, got refs:\n%s", backupRefsOutput)
	}

	// Also verify that the old commit exists in the converted serving repository!
	servingRefsOutput, err := runCmd(info.ServingPath, "git", "show-ref")
	if err != nil {
		t.Fatalf("failed to show-ref in serving repo: %v", err)
	}

	if !strings.Contains(servingRefsOutput, "refs/evergit-backups/heads/") {
		t.Errorf("Expected backup ref to exist in converted serving repository, got refs:\n%s", servingRefsOutput)
	}
}

func TestConverterBackupPreservation(t *testing.T) {
	// Create a temp workspace
	tempDir, err := os.MkdirTemp("", "evergit-test-preservation-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// 1. Create upstream SHA1 repo
	upstreamPath := filepath.Join(tempDir, "upstream.git")
	err = os.MkdirAll(upstreamPath, 0755)
	if err != nil {
		t.Fatalf("failed to create upstream dir: %v", err)
	}

	_, err = runCmd(upstreamPath, "git", "init", "--initial-branch=main", "--object-format=sha1")
	if err != nil {
		_, err = runCmd(upstreamPath, "git", "init", "--object-format=sha1")
		if err != nil {
			t.Fatalf("failed to init upstream: %v", err)
		}
	}

	// Configure git user
	_, _ = runCmd(upstreamPath, "git", "config", "user.name", "Test User")
	_, _ = runCmd(upstreamPath, "git", "config", "user.email", "test@example.com")

	// Create Commit A
	testFile := filepath.Join(upstreamPath, "test.txt")
	_ = os.WriteFile(testFile, []byte("hello commit A"), 0644)
	_, _ = runCmd(upstreamPath, "git", "add", "test.txt")
	_, _ = runCmd(upstreamPath, "git", "commit", "-m", "commit A")

	// 2. Setup the Converter Manager
	storageRoot := filepath.Join(tempDir, "storage")
	manager := NewManager(0) // Zero TTL syncs on every call

	info := &resolver.RepositoryInfo{
		Domain:      "local",
		Owner:       "test",
		RepoName:    "repo-preservation",
		RemoteURL:   upstreamPath,
		MirrorPath:  filepath.Join(storageRoot, "mirrors", "local", "test", "repo-preservation.git"),
		ServingPath: filepath.Join(storageRoot, "repos", "local", "test", "repo-preservation.git"),
		BuildsPath:  filepath.Join(storageRoot, "builds", "local", "test", "repo-preservation.git"),
	}

	// First JIT conversion (Commit A is imported)
	err = manager.EnsureRepo(context.Background(), info, nil)
	if err != nil {
		t.Fatalf("First EnsureRepo failed: %v", err)
	}

	// 3. Amend Commit A (Commit B) upstream to trigger force-push backup
	_ = os.WriteFile(testFile, []byte("hello commit B (force-pushed)"), 0644)
	_, _ = runCmd(upstreamPath, "git", "add", "test.txt")
	_, _ = runCmd(upstreamPath, "git", "commit", "--amend", "-m", "commit B (amended)")

	// Sync a second time
	err = manager.EnsureRepo(context.Background(), info, nil)
	if err != nil {
		t.Fatalf("Second EnsureRepo failed: %v", err)
	}

	// Verify backup ref exists
	refsOutput, err := runCmd(info.ServingPath, "git", "show-ref")
	if err != nil {
		t.Fatalf("failed to show-ref after backup: %v", err)
	}
	if !strings.Contains(refsOutput, "refs/evergit-backups/heads/") {
		t.Fatalf("Expected backup ref to exist, got refs:\n%s", refsOutput)
	}

	// Extract backup ref name
	var backupRef string
	lines := strings.Split(refsOutput, "\n")
	for _, line := range lines {
		if strings.Contains(line, "refs/evergit-backups/heads/") {
			parts := strings.SplitN(strings.TrimSpace(line), " ", 2)
			if len(parts) == 2 {
				backupRef = parts[1]
				break
			}
		}
	}

	// 4. Now perform a normal, non-force-push update (Commit C) on the upstream
	_ = os.WriteFile(testFile, []byte("hello commit C (regular update)"), 0644)
	_, _ = runCmd(upstreamPath, "git", "add", "test.txt")
	_, _ = runCmd(upstreamPath, "git", "commit", "-m", "commit C (regular update)")

	// Sync a third time
	err = manager.EnsureRepo(context.Background(), info, nil)
	if err != nil {
		t.Fatalf("Third EnsureRepo failed: %v", err)
	}

	// 5. Verify the backup reference of Commit A was PRESERVED and still exists!
	refsOutputAfterThirdSync, err := runCmd(info.ServingPath, "git", "show-ref")
	if err != nil {
		t.Fatalf("failed to show-ref after third sync: %v", err)
	}

	if !strings.Contains(refsOutputAfterThirdSync, backupRef) {
		t.Errorf("Backup ref %s was DELETED or lost during incremental update!\nRemaining refs:\n%s", backupRef, refsOutputAfterThirdSync)
	}

	// 6. The backup must also survive in the mirror, which is the source of truth for full conversions
	mirrorRefsOutput, err := runCmd(info.MirrorPath, "git", "show-ref")
	if err != nil {
		t.Fatalf("failed to show-ref in mirror after third sync: %v", err)
	}
	if !strings.Contains(mirrorRefsOutput, backupRef) {
		t.Errorf("Backup ref %s was pruned from the mirror by fetch --prune!\nRemaining mirror refs:\n%s", backupRef, mirrorRefsOutput)
	}

	// The regular fast-forward update (Commit C) must not have created an extra backup
	if n := strings.Count(mirrorRefsOutput, "refs/evergit-backups/"); n != 1 {
		t.Errorf("Expected exactly 1 backup ref after a fast-forward update, got %d:\n%s", n, mirrorRefsOutput)
	}

	// 7. Force a full re-conversion by dropping the serving repository; the backup must be rebuilt from the mirror
	if err := os.RemoveAll(info.ServingPath); err != nil {
		t.Fatalf("failed to remove serving repo: %v", err)
	}
	err = manager.EnsureRepo(context.Background(), info, nil)
	if err != nil {
		t.Fatalf("Full re-conversion EnsureRepo failed: %v", err)
	}

	refsOutputAfterFullConversion, err := runCmd(info.ServingPath, "git", "show-ref")
	if err != nil {
		t.Fatalf("failed to show-ref after full re-conversion: %v", err)
	}
	if !strings.Contains(refsOutputAfterFullConversion, backupRef) {
		t.Errorf("Backup ref %s was lost during full re-conversion!\nRemaining refs:\n%s", backupRef, refsOutputAfterFullConversion)
	}
}

// newTestUpstream creates a SHA1 upstream repository with a single commit.
// upstreamCount gives test upstreams distinct names: local submodules are identified by name.
var upstreamCount atomic.Int64

func newTestUpstream(t *testing.T) string {
	t.Helper()
	upstreamPath := filepath.Join(t.TempDir(), fmt.Sprintf("upstream%d.git", upstreamCount.Add(1)))
	if _, err := runCmd("", "git", "init", "--initial-branch=main", "--object-format=sha1", upstreamPath); err != nil {
		t.Fatalf("failed to init upstream: %v", err)
	}
	_, _ = runCmd(upstreamPath, "git", "config", "user.name", "Test User")
	_, _ = runCmd(upstreamPath, "git", "config", "user.email", "test@example.com")
	commitToUpstream(t, upstreamPath, "commit 0")
	return upstreamPath
}

func commitToUpstream(t *testing.T, upstreamPath, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(upstreamPath, "test.txt"), []byte(msg), 0644); err != nil {
		t.Fatal(err)
	}
	_, _ = runCmd(upstreamPath, "git", "add", "test.txt")
	if _, err := runCmd(upstreamPath, "git", "commit", "-m", msg); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}
}

func newTestRepoInfo(t *testing.T, upstreamPath string) *resolver.RepositoryInfo {
	storageRoot := t.TempDir()
	return &resolver.RepositoryInfo{
		Domain:      "local",
		Owner:       "test",
		RepoName:    "repo",
		RemoteURL:   upstreamPath,
		MirrorPath:  filepath.Join(storageRoot, "mirrors", "repo.git"),
		ServingPath: filepath.Join(storageRoot, "repos", "repo.git"),
		BuildsPath:  filepath.Join(storageRoot, "builds", "repo.git"),
	}
}

// assertCompatMapComplete checks that every commit in the serving repo has a loose-object-idx entry.
func assertCompatMapComplete(t *testing.T, servingPath string) {
	t.Helper()
	types, err := runCmd(servingPath, "git", "cat-file", "--batch-all-objects", "--batch-check=%(objecttype) %(objectname)")
	if err != nil {
		t.Fatal(err)
	}
	idx, err := os.ReadFile(filepath.Join(servingPath, "objects", "loose-object-idx"))
	if err != nil {
		t.Fatalf("loose-object-idx missing: %v", err)
	}
	for _, line := range strings.Split(types, "\n") {
		if oid, ok := strings.CutPrefix(line, "commit "); ok && !strings.Contains(string(idx), oid+" ") {
			t.Errorf("commit %s has no loose-object-idx entry", oid)
		}
	}
}

func TestCompatMapCompleteAfterIncrementalConversion(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	info := newTestRepoInfo(t, upstreamPath)
	manager := NewManager(0)

	if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("initial EnsureRepo failed: %v", err)
	}
	for i := 1; i <= 40; i++ {
		commitToUpstream(t, upstreamPath, fmt.Sprintf("commit %d", i))
	}
	if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("incremental EnsureRepo failed: %v", err)
	}
	assertCompatMapComplete(t, info.ServingPath)
}

func TestCompatMapRecoversWhenMissing(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	info := newTestRepoInfo(t, upstreamPath)
	manager := NewManager(0)

	if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("initial EnsureRepo failed: %v", err)
	}
	if err := os.Remove(filepath.Join(info.ServingPath, "objects", "loose-object-idx")); err != nil {
		t.Fatal(err)
	}

	// An incremental update must rebuild the map rather than fail to append to it
	commitToUpstream(t, upstreamPath, "commit 1")
	if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("incremental EnsureRepo failed: %v", err)
	}
	assertCompatMapComplete(t, info.ServingPath)

	// A sync with no upstream changes must also restore a lost map
	if err := os.Remove(filepath.Join(info.ServingPath, "objects", "loose-object-idx")); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("no-op EnsureRepo failed: %v", err)
	}
	assertCompatMapComplete(t, info.ServingPath)
}

func TestDeletedBranchIsNoLongerServed(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	_, _ = runCmd(upstreamPath, "git", "branch", "feature")
	info := newTestRepoInfo(t, upstreamPath)
	manager := NewManager(0)

	if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("initial EnsureRepo failed: %v", err)
	}

	// A backup that only survives in the serving repo (pruned from mirrors by older versions)
	mainOID, _ := runCmd(info.ServingPath, "git", "rev-parse", "refs/heads/main")
	legacyBackup := "refs/evergit-backups/heads/legacy/20260101-000000-abcdef12"
	if _, err := runCmd(info.ServingPath, "git", "update-ref", legacyBackup, mainOID); err != nil {
		t.Fatal(err)
	}

	if _, err := runCmd(upstreamPath, "git", "branch", "-D", "feature"); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("EnsureRepo after deletion failed: %v", err)
	}

	refs, err := runCmd(info.ServingPath, "git", "for-each-ref", "--format=%(refname)")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(refs, "refs/heads/feature\n") || strings.HasSuffix(refs, "refs/heads/feature") {
		t.Errorf("deleted branch is still served:\n%s", refs)
	}
	if !strings.Contains(refs, "refs/evergit-backups/heads/feature/") {
		t.Errorf("deleted branch was not archived:\n%s", refs)
	}
	if !strings.Contains(refs, legacyBackup) {
		t.Errorf("legacy backup %s was removed:\n%s", legacyBackup, refs)
	}
}

func TestFullConversionNeverHidesServingRepo(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	info := newTestRepoInfo(t, upstreamPath)
	manager := NewManager(0)

	if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("initial EnsureRepo failed: %v", err)
	}

	stop := make(chan struct{})
	failures := make(chan error, 1000)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := runCmd("", "git", "--git-dir="+info.ServingPath, "rev-parse", "HEAD"); err != nil {
				select {
				case failures <- err:
				default:
				}
			}
		}
	}()

	for i := 1; i <= 10; i++ {
		commitToUpstream(t, upstreamPath, fmt.Sprintf("commit %d", i))
		// Dropping the marker forces the full conversion path
		_ = os.Remove(filepath.Join(info.ServingPath, stateFile))
		if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
			t.Fatalf("EnsureRepo %d failed: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	close(failures)

	if n := len(failures); n > 0 {
		t.Errorf("%d reads failed during full conversions, first: %v", n, <-failures)
	}
	assertCompatMapComplete(t, info.ServingPath)

	// Only the current build should remain once older ones are cleaned up
	builds, err := os.ReadDir(info.BuildsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(builds) > 2 {
		t.Errorf("expected at most 2 builds (current and previous), found %d", len(builds))
	}
}

func TestRunCmdTimeout(t *testing.T) {
	start := time.Now()
	if _, err := runCmdTimeout(100*time.Millisecond, "", "sleep", "10"); err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timed-out command took %v to return", elapsed)
	}
}

func TestBrokenMirrorIsRecloned(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	info := newTestRepoInfo(t, upstreamPath)
	manager := NewManager(0)

	// An interrupted clone can leave an empty directory behind. Put it inside an unrelated
	// repository, which git would otherwise discover and operate on instead.
	parentRepo := filepath.Dir(filepath.Dir(info.MirrorPath))
	if _, err := runCmd("", "git", "init", "-q", parentRepo); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(info.MirrorPath, 0755); err != nil {
		t.Fatal(err)
	}

	if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("EnsureRepo with a broken mirror failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(info.MirrorPath, "HEAD")); err != nil {
		t.Errorf("mirror was not re-cloned: %v", err)
	}
	if out, _ := runCmd(parentRepo, "git", "config", "--get-all", "remote.origin.fetch"); out != "" {
		t.Errorf("unrelated parent repository was modified: remote.origin.fetch = %q", out)
	}
}

func TestLookupID(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "objects"), 0755); err != nil {
		t.Fatal(err)
	}
	a256, a1 := "aa"+strings.Repeat("1", 62), "ab12345"+strings.Repeat("1", 33)
	b256, b1 := "aa"+strings.Repeat("2", 62), "ab12399"+strings.Repeat("2", 33)
	idx := "# loose-object-idx\n" + a256 + " " + a1 + "\n" + b256 + " " + b1 + "\n"
	if err := os.WriteFile(filepath.Join(repo, "objects", "loose-object-idx"), []byte(idx), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		format, id, want string
		wantErr          error
	}{
		{"sha1", a1, a256, nil},
		{"sha1", "AB12345", a256, nil},
		{"sha1", "ab123", "", ErrIDInvalid}, // shorter than 7
		{"sha1", "ab12300", "", ErrIDNotFound},
		{"sha1", "ab1239", "", ErrIDInvalid},
		{"sha256", b256, b1, nil},
		{"sha256", "aa22222", b1, nil},
		{"sha256", "aaaaaaa", "", ErrIDNotFound},
		{"sha1", a1 + "0", "", ErrIDInvalid}, // longer than a SHA-1
		{"sha256", "aa", "", ErrIDInvalid},
		{"md5", a1, "", ErrIDInvalid},
	}
	for _, tt := range tests {
		got, err := LookupID(repo, tt.format, tt.id)
		if !errors.Is(err, tt.wantErr) || got != tt.want {
			t.Errorf("LookupID(%s, %s) = %q, %v; want %q, %v", tt.format, tt.id, got, err, tt.want, tt.wantErr)
		}
	}

	// Two SHA-1s share the prefix "ab123"; make it long enough to be valid but still ambiguous
	if err := os.WriteFile(filepath.Join(repo, "objects", "loose-object-idx"),
		[]byte(idx+"cc"+strings.Repeat("3", 62)+" ab12345"+strings.Repeat("3", 33)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LookupID(repo, "sha1", "ab12345"); !errors.Is(err, ErrIDAmbiguous) {
		t.Errorf("ambiguous prefix: err = %v, want ErrIDAmbiguous", err)
	}
}

// stalledWriter blocks every write until released, like an SSH client that stopped reading stderr.
type stalledWriter struct{ release chan struct{} }

func (w stalledWriter) Write(p []byte) (int, error) {
	<-w.release
	return len(p), nil
}

func TestStalledProgressWriterDoesNotBlockConversion(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	info := newTestRepoInfo(t, upstreamPath)
	manager := NewManager(0)
	writer := stalledWriter{release: make(chan struct{})}
	defer close(writer.release)

	done := make(chan error, 1)
	go func() { done <- manager.EnsureRepo(context.Background(), info, writer) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("EnsureRepo failed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("EnsureRepo blocked on a stalled progress writer while holding the repository lock")
	}
}

func TestRepoLocksAreReleased(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	manager := NewManager(0)

	for i := 0; i < 5; i++ {
		if err := manager.EnsureRepo(context.Background(), newTestRepoInfo(t, upstreamPath), nil); err != nil {
			t.Fatalf("EnsureRepo failed: %v", err)
		}
	}
	// Failing requests (unreachable upstream) must not leak entries either
	for i := 0; i < 5; i++ {
		_ = manager.EnsureRepo(context.Background(), newTestRepoInfo(t, filepath.Join(t.TempDir(), "missing")), nil)
	}

	manager.locksMu.Lock()
	defer manager.locksMu.Unlock()
	if n := len(manager.repoLocks); n != 0 {
		t.Errorf("%d repository locks retained after all requests finished", n)
	}
}

func TestInheritedGitEnvironmentIsIgnored(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	info := newTestRepoInfo(t, upstreamPath)
	decoy := filepath.Join(t.TempDir(), "decoy.git")
	if _, err := runCmd("", "git", "init", "-q", "--bare", decoy); err != nil {
		t.Fatal(err)
	}

	// A stray variable in the daemon's environment would redirect every git operation
	t.Setenv("GIT_DIR", decoy)
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(decoy, "objects"))
	if err := NewManager(0).EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("EnsureRepo failed with GIT_DIR set: %v", err)
	}
	os.Unsetenv("GIT_DIR")
	os.Unsetenv("GIT_OBJECT_DIRECTORY")

	if refs, _ := runCmd(decoy, "git", "--git-dir=.", "for-each-ref"); refs != "" {
		t.Errorf("decoy repository was modified:\n%s", refs)
	}
	assertCompatMapComplete(t, info.ServingPath)
}

func assertOnlyBranchesAndTags(t *testing.T, label, repoPath string) {
	t.Helper()
	refs, err := runCmd(repoPath, "git", "--git-dir=.", "for-each-ref", "--format=%(refname)")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range strings.Split(refs, "\n") {
		if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") && !strings.HasPrefix(ref, "refs/evergit-backups/") {
			t.Errorf("%s contains unexpected ref %s", label, ref)
		}
	}
}

func TestOnlyBranchesAndTagsAreMirrored(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	head, _ := runCmd(upstreamPath, "git", "rev-parse", "HEAD")
	_, _ = runCmd(upstreamPath, "git", "tag", "v1.0")
	// Forge-specific refs, e.g. GitHub pull requests from forks
	_, _ = runCmd(upstreamPath, "git", "update-ref", "refs/pull/1/head", head)
	_, _ = runCmd(upstreamPath, "git", "update-ref", "refs/merge-requests/1/head", head)

	info := newTestRepoInfo(t, upstreamPath)
	mgr := NewManager(0)
	if err := mgr.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("EnsureRepo failed: %v", err)
	}
	assertOnlyBranchesAndTags(t, "mirror", info.MirrorPath)
	assertOnlyBranchesAndTags(t, "serving repo", info.ServingPath)
	if refs, _ := runCmd(info.ServingPath, "git", "--git-dir=.", "for-each-ref", "--format=%(refname)"); !strings.Contains(refs, "refs/tags/v1.0") {
		t.Errorf("tag missing from serving repo:\n%s", refs)
	}

	// Incremental fetch: upstream adds another branch, tag, and forge ref
	commitToUpstream(t, upstreamPath, "second commit")
	head2, _ := runCmd(upstreamPath, "git", "rev-parse", "HEAD")
	_, _ = runCmd(upstreamPath, "git", "tag", "v2.0")
	_, _ = runCmd(upstreamPath, "git", "update-ref", "refs/pull/2/head", head2)

	if err := mgr.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("EnsureRepo incremental failed: %v", err)
	}
	assertOnlyBranchesAndTags(t, "mirror after fetch", info.MirrorPath)
	assertOnlyBranchesAndTags(t, "serving repo after fetch", info.ServingPath)
	if refs, _ := runCmd(info.ServingPath, "git", "--git-dir=.", "for-each-ref", "--format=%(refname)"); !strings.Contains(refs, "refs/tags/v2.0") {
		t.Errorf("tag v2.0 missing from serving repo:\n%s", refs)
	}
}

func TestGitlinkWithoutURLNeedsApproval(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	sha1 := strings.Repeat("ab", 20)
	// A gitlink records a SHA-1 commit of another repository, here without a .gitmodules entry
	if _, err := runCmd(upstreamPath, "git", "update-index", "--add", "--cacheinfo", "160000,"+sha1+",libs/dependency"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(upstreamPath, "git", "commit", "-q", "-m", "add submodule"); err != nil {
		t.Fatal(err)
	}

	info := newTestRepoInfo(t, upstreamPath)
	manager := NewManager(0)
	assertNeedsApproval(t, manager.EnsureRepo(context.Background(), info, nil), "local/test/repo", "path:libs/dependency")

	approveSynthetic(t, manager, "local/test/repo path:libs/dependency")
	if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("EnsureRepo failed for repository with approved submodule: %v", err)
	}
	out, err := runCmd(info.ServingPath, "git", "ls-tree", "HEAD", "libs/dependency")
	if err != nil {
		t.Fatalf("git ls-tree failed: %v", err)
	}
	expectedHash := sha1 + strings.Repeat("0", 24)
	if !strings.Contains(out, expectedHash) {
		t.Errorf("expected gitlink %s in ls-tree output, got: %q", expectedHash, out)
	}
}

func TestSubmodulePreConversionWithRealSHA256(t *testing.T) {
	sub := newTestUpstream(t)
	subCommit := gitLine(t, sub, nil, "rev-parse", "HEAD")
	super := newTestUpstream(t)
	addGitlink(t, super, "libs/sub", urlOf(sub), subCommit)

	m := newSubmoduleManager(t.TempDir(), 0, super, sub)
	info := repoInfo(t, m, super)
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("EnsureRepo failed for superproject: %v", err)
	}

	// The submodule was converted, and the gitlink records its real SHA-256 commit
	realSHA256, err := LookupID(repoInfo(t, m, sub).ServingPath, "sha1", subCommit)
	if err != nil {
		t.Fatalf("expected to lookup real SHA256 of submodule commit %s: %v", subCommit, err)
	}
	if got := gitlinkOf(t, info.ServingPath, "HEAD", "libs/sub"); got != realSHA256 {
		t.Errorf("gitlink = %s, want the real SHA-256 %s", got, realSHA256)
	}
}

func TestSubmodulePathMovedInHistoryWithRealSHA256(t *testing.T) {
	sub := newTestUpstream(t)
	subCommit := gitLine(t, sub, nil, "rev-parse", "HEAD")
	super := newTestUpstream(t)

	// Commit 1: submodule at src/sub
	addGitlink(t, super, "src/sub", urlOf(sub), subCommit)
	commit1 := gitLine(t, super, nil, "rev-parse", "HEAD")

	// Commit 2: moved to subprojects/sub, plus the same commit at a path without .gitmodules entry
	gitRaw(t, super, nil, "rm", "-q", "--cached", "src/sub")
	gitRaw(t, super, nil, "config", "-f", ".gitmodules", "--remove-section", "submodule.src/sub")
	gitRaw(t, super, nil, "config", "-f", ".gitmodules", "submodule.sub.path", "subprojects/sub")
	gitRaw(t, super, nil, "config", "-f", ".gitmodules", "submodule.sub.url", urlOf(sub))
	gitRaw(t, super, nil, "add", ".gitmodules")
	gitRaw(t, super, nil, "update-index", "--add", "--cacheinfo", "160000,"+subCommit+",subprojects/sub")
	gitRaw(t, super, nil, "update-index", "--add", "--cacheinfo", "160000,"+subCommit+",unlisted/sub")
	gitRaw(t, super, nil, "commit", "-q", "-m", "move submodule to subprojects/sub")

	m := newSubmoduleManager(t.TempDir(), 0, super, sub)
	info := repoInfo(t, m, super)
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("EnsureRepo failed for superproject: %v", err)
	}
	realSHA256, err := LookupID(repoInfo(t, m, sub).ServingPath, "sha1", subCommit)
	if err != nil {
		t.Fatalf("expected real SHA-256 for submodule commit %s: %v", subCommit, err)
	}

	// The same submodule commit gets the same real SHA-256 at every path, also in history
	converted1, err := LookupID(info.ServingPath, "sha1", commit1)
	if err != nil {
		t.Fatal(err)
	}
	for _, gitlink := range []struct{ rev, path string }{{"HEAD", "subprojects/sub"}, {"HEAD", "unlisted/sub"}, {converted1, "src/sub"}} {
		if got := gitlinkOf(t, info.ServingPath, gitlink.rev, gitlink.path); got != realSHA256 {
			t.Errorf("%s:%s = %s, want the real SHA-256 %s", gitlink.rev, gitlink.path, got, realSHA256)
		}
	}
}

func TestSubmoduleRecursionDepthLimit(t *testing.T) {
	sub := newTestUpstream(t)
	super := newTestUpstream(t)
	addGitlink(t, super, "libs/sub", urlOf(sub), gitLine(t, sub, nil, "rev-parse", "HEAD"))

	m := newSubmoduleManager(t.TempDir(), 0, super, sub)
	m.SetMaxSubmoduleDepth(0)

	// Exceeding the depth fails rather than recording the gitlink with the synthetic SHA-256, which
	// would depend on how the repository was reached
	err := m.EnsureRepo(context.Background(), repoInfo(t, m, super), nil)
	if !errors.Is(err, ErrSubmoduleDepth) {
		t.Fatalf("EnsureRepo with depth 0 = %v, want ErrSubmoduleDepth", err)
	}
}

func TestSubmoduleCycleFails(t *testing.T) {
	// Repository A records B as a submodule, and B records A
	repoA := newTestUpstream(t)
	repoB := newTestUpstream(t)
	shaA := gitLine(t, repoA, nil, "rev-parse", "HEAD")
	shaB := gitLine(t, repoB, nil, "rev-parse", "HEAD")
	addGitlink(t, repoA, "subB", urlOf(repoB), shaB)
	addGitlink(t, repoB, "subA", urlOf(repoA), shaA)

	m := newSubmoduleManager(t.TempDir(), 0, repoA, repoB)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Fails promptly instead of deadlocking on the conversion further up the chain
	err := m.EnsureRepo(ctx, repoInfo(t, m, repoA), nil)
	if !errors.Is(err, ErrSubmoduleCycle) {
		t.Fatalf("EnsureRepo with circular submodules = %v, want ErrSubmoduleCycle", err)
	}
}

func TestNestedTagsConvertSuccessfully(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	// Create an annotated tag and a nested tag pointing to it
	if _, err := runCmd(upstreamPath, "git", "tag", "-a", "v1.0", "-m", "tag v1.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(upstreamPath, "git", "tag", "-a", "v2.0", "v1.0", "-m", "nested tag v2.0"); err != nil {
		t.Fatal(err)
	}

	info := newTestRepoInfo(t, upstreamPath)
	err := NewManager(0).EnsureRepo(context.Background(), info, nil)
	if err != nil {
		t.Fatalf("EnsureRepo failed for repository with nested tags: %v", err)
	}

	// Verify both tags exist in serving repo
	out, err := runCmd(info.ServingPath, "git", "tag")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "v1.0") || !strings.Contains(out, "v2.0") {
		t.Errorf("expected both tags in converted repo, got:\n%s", out)
	}
}

func newTestUpstreamSHA256(t *testing.T) string {
	t.Helper()
	upstreamPath := filepath.Join(t.TempDir(), "upstream.git")
	if _, err := runCmd("", "git", "init", "--initial-branch=main", "--object-format=sha256", upstreamPath); err != nil {
		t.Fatalf("failed to init sha256 upstream: %v", err)
	}
	_, _ = runCmd(upstreamPath, "git", "config", "user.name", "Test User")
	_, _ = runCmd(upstreamPath, "git", "config", "user.email", "test@example.com")
	commitToUpstream(t, upstreamPath, "commit 0")
	return upstreamPath
}

func TestNativeSHA256UpstreamIsServedUnconverted(t *testing.T) {
	upstreamPath := newTestUpstreamSHA256(t)
	upstreamHead, err := runCmd(upstreamPath, "git", "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("failed to get upstream HEAD: %v", err)
	}

	info := newTestRepoInfo(t, upstreamPath)
	manager := NewManager(0)

	// Initial sync
	if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("initial EnsureRepo failed: %v", err)
	}

	// Verify serving repo has sha256 format
	format, err := runCmd(info.ServingPath, "git", "--git-dir=.", "rev-parse", "--show-object-format")
	if err != nil || format != "sha256" {
		t.Fatalf("serving repo format = %q (err %v), want sha256", format, err)
	}

	// Verify serving HEAD commit hash matches upstream HEAD commit hash directly
	servingHead, err := runCmd(info.ServingPath, "git", "--git-dir=.", "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("failed to get serving HEAD: %v", err)
	}
	if servingHead != upstreamHead {
		t.Fatalf("serving HEAD %q != upstream HEAD %q", servingHead, upstreamHead)
	}

	// Test incremental sync
	commitToUpstream(t, upstreamPath, "commit 1")
	newUpstreamHead, err := runCmd(upstreamPath, "git", "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("failed to get updated upstream HEAD: %v", err)
	}
	if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("incremental EnsureRepo failed: %v", err)
	}
	newServingHead, err := runCmd(info.ServingPath, "git", "--git-dir=.", "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("failed to get updated serving HEAD: %v", err)
	}
	if newServingHead != newUpstreamHead {
		t.Fatalf("updated serving HEAD %q != upstream HEAD %q", newServingHead, newUpstreamHead)
	}

	// Test branch deletion and archival on native SHA-256
	_, _ = runCmd(upstreamPath, "git", "branch", "feature", newUpstreamHead)
	if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("EnsureRepo for new branch failed: %v", err)
	}
	// Delete branch upstream
	_, _ = runCmd(upstreamPath, "git", "branch", "-D", "feature")
	if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("EnsureRepo after branch deletion failed: %v", err)
	}
	servingRefs, err := runCmd(info.ServingPath, "git", "--git-dir=.", "for-each-ref", "--format=%(refname)")
	if err != nil {
		t.Fatalf("failed to read serving refs: %v", err)
	}
	if strings.Contains(servingRefs, "refs/heads/feature") {
		t.Errorf("deleted branch feature still present in serving refs:\n%s", servingRefs)
	}
	if !strings.Contains(servingRefs, "refs/evergit-backups/heads/feature/") {
		t.Errorf("deleted branch feature not archived in refs/evergit-backups/:\n%s", servingRefs)
	}

	// Verify that backup refs in serving repo are preserved even if the mirror loses them
	// (e.g. mirror was re-cloned after corruption)
	mirrorRefs, _ := runCmd(info.MirrorPath, "git", "for-each-ref", "--format=%(refname)")
	for _, ref := range strings.Split(mirrorRefs, "\n") {
		if strings.HasPrefix(ref, "refs/evergit-backups/") {
			_, _ = runCmd(info.MirrorPath, "git", "update-ref", "-d", ref)
		}
	}
	commitToUpstream(t, upstreamPath, "commit 2")
	if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("EnsureRepo after mirror backup loss failed: %v", err)
	}
	servingRefsAfter, err := runCmd(info.ServingPath, "git", "--git-dir=.", "for-each-ref", "--format=%(refname)")
	if err != nil {
		t.Fatalf("failed to read serving refs after sync: %v", err)
	}
	if !strings.Contains(servingRefsAfter, "refs/evergit-backups/heads/feature/") {
		t.Errorf("backup ref was pruned from serving repo when mirror lost it:\n%s", servingRefsAfter)
	}
}

func TestConversionEmitsProgress(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	info := newTestRepoInfo(t, upstreamPath)
	manager := NewManager(0)

	var progressBuf strings.Builder
	if err := manager.EnsureRepo(context.Background(), info, &progressBuf); err != nil {
		t.Fatalf("EnsureRepo failed: %v", err)
	}

	progressOutput := progressBuf.String()
	if !strings.Contains(progressOutput, "Converting SHA-1 objects to SHA-256...") {
		t.Errorf("expected initial conversion notice in progress output, got:\n%s", progressOutput)
	}
}

func TestConversionConcurrencyLimiter(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	info := newTestRepoInfo(t, upstreamPath)

	manager := NewManager(0, 1)
	manager.SetConversionTimeout(10 * time.Millisecond)

	// Acquire the single conversion slot
	if !manager.TryAcquireConversionSlot() {
		t.Fatal("expected slot acquisition to succeed")
	}

	// EnsureRepo must time out waiting for slot and return ErrServerBusy
	err := manager.EnsureRepo(context.Background(), info, nil)
	if !errors.Is(err, ErrServerBusy) {
		t.Fatalf("expected ErrServerBusy when semaphore is full, got: %v", err)
	}

	// Release slot
	manager.ReleaseConversionSlot()

	// EnsureRepo should now proceed and succeed
	if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("expected EnsureRepo to succeed after slot release: %v", err)
	}
}

func TestDiskSpaceSafeguard(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	info := newTestRepoInfo(t, upstreamPath)

	manager := NewManager(0)
	// Require an impossible amount of free space (1 Exabyte)
	manager.SetMinFreeDisk(1000 * 1024 * 1024 * 1024 * 1024 * 1024)

	err := manager.EnsureRepo(context.Background(), info, nil)
	if !errors.Is(err, ErrDiskSpaceExhausted) {
		t.Fatalf("expected ErrDiskSpaceExhausted when disk threshold is breached, got: %v", err)
	}

	// Reset to 0 (disabled) and verify clone succeeds
	manager.SetMinFreeDisk(0)
	if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("expected EnsureRepo to succeed when threshold disabled: %v", err)
	}
}

func TestConversionContinuesAfterClientsDisconnect(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	info := newTestRepoInfo(t, upstreamPath)
	manager := NewManager(0)

	// The only client is gone at once, e.g. dropped by a proxy timeout
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.EnsureRepo(ctx, info, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}

	// The conversion still finishes, so a long one is not restarted by every request
	manager.WaitIdle()
	if !convertedUpToDate(info) {
		t.Error("conversion did not finish after its client disconnected")
	}
}

func TestEnsureRepoWithinBoundsTheWait(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	info := newTestRepoInfo(t, upstreamPath)
	manager := NewManager(0, 1)
	manager.SetConversionTimeout(10 * time.Second)

	// A slow first conversion (waiting for a conversion slot): reported as in progress
	if !manager.TryAcquireConversionSlot() {
		t.Fatal("no conversion slot")
	}
	err := manager.EnsureRepoWithin(context.Background(), info, nil, 100*time.Millisecond)
	if !errors.Is(err, ErrConversionInProgress) {
		t.Fatalf("EnsureRepoWithin during a first conversion = %v, want ErrConversionInProgress", err)
	}
	manager.ReleaseConversionSlot()
	manager.WaitIdle()
	if !convertedUpToDate(info) {
		t.Fatal("conversion did not finish in the background")
	}

	// A slow update: the current copy is served meanwhile
	commitToUpstream(t, upstreamPath, "commit 1")
	if !manager.TryAcquireConversionSlot() {
		t.Fatal("no conversion slot")
	}
	start := time.Now()
	if err := manager.EnsureRepoWithin(context.Background(), info, nil, 100*time.Millisecond); err != nil {
		t.Fatalf("EnsureRepoWithin during an update = %v, want the current copy served", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("waited %v for the update", waited)
	}
	manager.ReleaseConversionSlot()
	manager.WaitIdle()
	if !convertedUpToDate(info) {
		t.Error("update did not finish in the background")
	}
}

func TestProgressWrittenAfterClientLeftIsDropped(t *testing.T) {
	var out strings.Builder
	w := newAsyncWriter(&out)
	w.Close()
	if _, err := w.Write([]byte("late progress\n")); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureRepoContinuesForRemainingWaiters(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	info := newTestRepoInfo(t, upstreamPath)
	manager := NewManager(0)

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2 := context.Background()

	errChan1 := make(chan error, 1)
	errChan2 := make(chan error, 1)

	go func() { errChan1 <- manager.EnsureRepo(ctx1, info, nil) }()
	time.Sleep(10 * time.Millisecond)
	go func() { errChan2 <- manager.EnsureRepo(ctx2, info, nil) }()

	time.Sleep(10 * time.Millisecond)
	cancel1() // cancel client 1

	err1 := <-errChan1
	if !errors.Is(err1, context.Canceled) {
		t.Errorf("expected client 1 to be canceled, got: %v", err1)
	}

	err2 := <-errChan2
	if err2 != nil {
		t.Errorf("expected client 2 to succeed, got: %v", err2)
	}

	manager.WaitIdle()
}

func TestConverterFileReplacedByDirectory(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "evergit-test-file-to-dir-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	upstreamPath := filepath.Join(tempDir, "upstream.git")
	if err := os.MkdirAll(upstreamPath, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(upstreamPath, "git", "init", "--initial-branch=main", "--object-format=sha1"); err != nil {
		if _, err := runCmd(upstreamPath, "git", "init", "--object-format=sha1"); err != nil {
			t.Fatalf("failed to init upstream SHA1 repo: %v", err)
		}
	}
	_, _ = runCmd(upstreamPath, "git", "config", "user.name", "Test User")
	_, _ = runCmd(upstreamPath, "git", "config", "user.email", "test@example.com")

	// Commit 1: create a symlink "system -> ." inside a subdirectory
	testDir := filepath.Join(upstreamPath, "tests")
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(testDir, "system")
	if err := os.Symlink(".", symlinkPath); err != nil {
		t.Fatal(err)
	}
	_, _ = runCmd(upstreamPath, "git", "add", "tests/system")
	if _, err := runCmd(upstreamPath, "git", "commit", "-m", "commit 1: system is symlink"); err != nil {
		t.Fatalf("commit 1 failed: %v", err)
	}

	// Commit 2: delete symlink "system", replace with directory "system" containing "cpu" and "node"
	if err := os.Remove(symlinkPath); err != nil {
		t.Fatal(err)
	}
	nodeDir := filepath.Join(testDir, "system", "node")
	if err := os.MkdirAll(nodeDir, 0755); err != nil {
		t.Fatal(err)
	}
	onlineFile := filepath.Join(nodeDir, "online")
	if err := os.WriteFile(onlineFile, []byte("0\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cpuDir := filepath.Join(testDir, "system", "cpu", "cpu0")
	if err := os.MkdirAll(cpuDir, 0755); err != nil {
		t.Fatal(err)
	}
	cpuNodeLink := filepath.Join(cpuDir, "node0")
	if err := os.Symlink("../../node/node0", cpuNodeLink); err != nil {
		t.Fatal(err)
	}

	_, _ = runCmd(upstreamPath, "git", "add", "-A")
	if _, err := runCmd(upstreamPath, "git", "commit", "-m", "commit 2: replace symlink with directories"); err != nil {
		t.Fatalf("commit 2 failed: %v", err)
	}

	// Convert repository with Evergit
	storageRoot := filepath.Join(tempDir, "storage")
	manager := NewManager(0)
	info := &resolver.RepositoryInfo{
		Domain:      "local",
		Owner:       "test",
		RepoName:    "repo-file-to-dir",
		RemoteURL:   upstreamPath,
		MirrorPath:  filepath.Join(storageRoot, "mirrors", "local", "test", "repo-file-to-dir.git"),
		ServingPath: filepath.Join(storageRoot, "repos", "local", "test", "repo-file-to-dir.git"),
		BuildsPath:  filepath.Join(storageRoot, "builds", "local", "test", "repo-file-to-dir.git"),
	}

	if err := manager.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("EnsureRepo failed: %v", err)
	}

	// Verify that in the converted SHA-256 repository:
	// 1. tests/system/node/online exists and can be read
	outOnline, err := runCmd(info.ServingPath, "git", "show", "HEAD:tests/system/node/online")
	if err != nil {
		t.Fatalf("tests/system/node/online was wiped out during conversion: %v (git show output: %q)", err, outOnline)
	}
	if strings.TrimSpace(outOnline) != "0" {
		t.Errorf("expected '0', got %q", outOnline)
	}

	// 2. tests/system/cpu/cpu0/node0 exists
	outLink, err := runCmd(info.ServingPath, "git", "show", "HEAD:tests/system/cpu/cpu0/node0")
	if err != nil {
		t.Fatalf("tests/system/cpu/cpu0/node0 was wiped out during conversion: %v (git show output: %q)", err, outLink)
	}
	if strings.TrimSpace(outLink) != "../../node/node0" {
		t.Errorf("expected symlink target '../../node/node0', got %q", outLink)
	}

	// 3. ls-tree HEAD:tests/system shows both cpu and node trees
	lsOut, err := runCmd(info.ServingPath, "git", "ls-tree", "HEAD:tests/system")
	if err != nil {
		t.Fatalf("failed to ls-tree tests/system: %v", err)
	}
	if !strings.Contains(lsOut, "cpu") || !strings.Contains(lsOut, "node") {
		t.Errorf("expected tests/system to contain both cpu and node trees, got:\n%s", lsOut)
	}
}
