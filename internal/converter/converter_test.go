package converter

import (
	"os"
	"path/filepath"
	"strings"
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
}
