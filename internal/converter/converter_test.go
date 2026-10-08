package converter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	manager := NewManager(storageRoot, 1*time.Second)

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
	err = manager.EnsureRepo(info, nil)
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
	manager := NewManager(storageRoot, 10*time.Millisecond) // Low TTL to force sync on next check

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
	err = manager.EnsureRepo(info, nil)
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

	// Wait for TTL to expire to force a sync
	time.Sleep(20 * time.Millisecond)

	// Second JIT conversion (Triggers fetch and backup check!)
	err = manager.EnsureRepo(info, nil)
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
	manager := NewManager(storageRoot, 10*time.Millisecond) // Low TTL to force sync

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
	err = manager.EnsureRepo(info, nil)
	if err != nil {
		t.Fatalf("First EnsureRepo failed: %v", err)
	}

	// 3. Amend Commit A (Commit B) upstream to trigger force-push backup
	_ = os.WriteFile(testFile, []byte("hello commit B (force-pushed)"), 0644)
	_, _ = runCmd(upstreamPath, "git", "add", "test.txt")
	_, _ = runCmd(upstreamPath, "git", "commit", "--amend", "-m", "commit B (amended)")

	// Wait for TTL to expire and sync a second time
	time.Sleep(20 * time.Millisecond)
	err = manager.EnsureRepo(info, nil)
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

	// Wait for TTL to expire and sync a third time
	time.Sleep(20 * time.Millisecond)
	err = manager.EnsureRepo(info, nil)
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
	err = manager.EnsureRepo(info, nil)
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
func newTestUpstream(t *testing.T) string {
	t.Helper()
	upstreamPath := filepath.Join(t.TempDir(), "upstream.git")
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
	manager := NewManager("", 0)

	if err := manager.EnsureRepo(info, nil); err != nil {
		t.Fatalf("initial EnsureRepo failed: %v", err)
	}
	for i := 1; i <= 40; i++ {
		commitToUpstream(t, upstreamPath, fmt.Sprintf("commit %d", i))
	}
	if err := manager.EnsureRepo(info, nil); err != nil {
		t.Fatalf("incremental EnsureRepo failed: %v", err)
	}
	assertCompatMapComplete(t, info.ServingPath)
}

func TestCompatMapRecoversWhenMissing(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	info := newTestRepoInfo(t, upstreamPath)
	manager := NewManager("", 0)

	if err := manager.EnsureRepo(info, nil); err != nil {
		t.Fatalf("initial EnsureRepo failed: %v", err)
	}
	if err := os.Remove(filepath.Join(info.ServingPath, "objects", "loose-object-idx")); err != nil {
		t.Fatal(err)
	}

	// An incremental update must rebuild the map rather than fail to append to it
	commitToUpstream(t, upstreamPath, "commit 1")
	if err := manager.EnsureRepo(info, nil); err != nil {
		t.Fatalf("incremental EnsureRepo failed: %v", err)
	}
	assertCompatMapComplete(t, info.ServingPath)

	// A sync with no upstream changes must also restore a lost map
	if err := os.Remove(filepath.Join(info.ServingPath, "objects", "loose-object-idx")); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureRepo(info, nil); err != nil {
		t.Fatalf("no-op EnsureRepo failed: %v", err)
	}
	assertCompatMapComplete(t, info.ServingPath)
}

func TestDeletedBranchIsNoLongerServed(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	_, _ = runCmd(upstreamPath, "git", "branch", "feature")
	info := newTestRepoInfo(t, upstreamPath)
	manager := NewManager("", 0)

	if err := manager.EnsureRepo(info, nil); err != nil {
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
	if err := manager.EnsureRepo(info, nil); err != nil {
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

func assertCompatObjectFormat(t *testing.T, servingPath string) {
	t.Helper()
	format, err := runCmd(servingPath, "git", "--git-dir=.", "config", "extensions.compatObjectFormat")
	if err != nil || format != "sha1" {
		t.Errorf("extensions.compatObjectFormat = %q (err %v), want sha1", format, err)
	}
}

func TestCompatObjectFormatRestoredAfterFailedImport(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	info := newTestRepoInfo(t, upstreamPath)
	manager := NewManager("", 0)

	if err := manager.EnsureRepo(info, nil); err != nil {
		t.Fatalf("initial EnsureRepo failed: %v", err)
	}

	// Corrupt the marks so the incremental fast-import fails
	if err := os.WriteFile(filepath.Join(info.ServingPath, "evergit-sha256-marks.txt"), []byte("garbage\n"), 0644); err != nil {
		t.Fatal(err)
	}
	commitToUpstream(t, upstreamPath, "commit 1")
	err := manager.EnsureRepo(info, nil)
	if err == nil {
		t.Fatal("expected incremental EnsureRepo to fail with corrupted marks")
	}
	if strings.Contains(err.Error(), `stderr: ""`) {
		t.Errorf("fast-import failure should include its stderr, got: %v", err)
	}
	assertCompatObjectFormat(t, info.ServingPath)
}

func TestCompatObjectFormatRestoredOnNoOpSync(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	info := newTestRepoInfo(t, upstreamPath)
	manager := NewManager("", 0)

	if err := manager.EnsureRepo(info, nil); err != nil {
		t.Fatalf("initial EnsureRepo failed: %v", err)
	}
	// Simulate a crash during a previous incremental conversion
	if _, err := runCmd(info.ServingPath, "git", "--git-dir=.", "config", "--unset", "extensions.compatObjectFormat"); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureRepo(info, nil); err != nil {
		t.Fatalf("no-op EnsureRepo failed: %v", err)
	}
	assertCompatObjectFormat(t, info.ServingPath)
}

func TestFullConversionNeverHidesServingRepo(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	info := newTestRepoInfo(t, upstreamPath)
	manager := NewManager("", 0)

	if err := manager.EnsureRepo(info, nil); err != nil {
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
		// Dropping the marks forces the full conversion path
		_ = os.Remove(filepath.Join(info.ServingPath, "evergit-sha1-marks.txt"))
		if err := manager.EnsureRepo(info, nil); err != nil {
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

func TestFullConversionMigratesLegacyServingDirectory(t *testing.T) {
	upstreamPath := newTestUpstream(t)
	info := newTestRepoInfo(t, upstreamPath)
	manager := NewManager("", 0)

	if err := manager.EnsureRepo(info, nil); err != nil {
		t.Fatalf("initial EnsureRepo failed: %v", err)
	}

	// Recreate the pre-builds layout: a real directory at ServingPath
	build, err := filepath.EvalSymlinks(info.ServingPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(info.ServingPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(build, info.ServingPath); err != nil {
		t.Fatal(err)
	}

	commitToUpstream(t, upstreamPath, "commit 1")
	_ = os.Remove(filepath.Join(info.ServingPath, "evergit-sha1-marks.txt"))
	if err := manager.EnsureRepo(info, nil); err != nil {
		t.Fatalf("EnsureRepo on legacy layout failed: %v", err)
	}

	fi, err := os.Lstat(info.ServingPath)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected ServingPath to be a symlink after migration (err %v)", err)
	}
	logOut, err := runCmd(info.ServingPath, "git", "--git-dir=.", "log", "--oneline", "-1")
	if err != nil || !strings.Contains(logOut, "commit 1") {
		t.Errorf("expected migrated repo to serve the latest commit, got %q (err %v)", logOut, err)
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
	manager := NewManager("", 0)

	// An interrupted clone can leave an empty directory behind. Put it inside an unrelated
	// repository, which git would otherwise discover and operate on instead.
	parentRepo := filepath.Dir(filepath.Dir(info.MirrorPath))
	if _, err := runCmd("", "git", "init", "-q", parentRepo); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(info.MirrorPath, 0755); err != nil {
		t.Fatal(err)
	}

	if err := manager.EnsureRepo(info, nil); err != nil {
		t.Fatalf("EnsureRepo with a broken mirror failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(info.MirrorPath, "HEAD")); err != nil {
		t.Errorf("mirror was not re-cloned: %v", err)
	}
	if out, _ := runCmd(parentRepo, "git", "config", "--get-all", "remote.origin.fetch"); out != "" {
		t.Errorf("unrelated parent repository was modified: remote.origin.fetch = %q", out)
	}
}
