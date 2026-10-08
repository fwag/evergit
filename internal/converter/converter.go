package converter

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"evergit/internal/resolver"
)

type Manager struct {
	storageRoot string
	cacheTTL    time.Duration
	locksMu     sync.Mutex
	repoLocks   map[string]*sync.Mutex
}

func NewManager(storageRoot string, cacheTTL time.Duration) *Manager {
	return &Manager{
		storageRoot: storageRoot,
		cacheTTL:    cacheTTL,
		repoLocks:   make(map[string]*sync.Mutex),
	}
}

// EnsureRepo checks if the repository exists, is up-to-date, and is converted.
// If not, it clones/fetches, converts it, and optionally streams progress.
func (m *Manager) EnsureRepo(info *resolver.RepositoryInfo, progressWriter io.Writer) error {
	// Get or create a per-repo lock
	m.locksMu.Lock()
	lock, exists := m.repoLocks[info.ServingPath]
	if !exists {
		lock = &sync.Mutex{}
		m.repoLocks[info.ServingPath] = lock
	}
	m.locksMu.Unlock()

	lock.Lock()
	defer lock.Unlock()

	// 1. Check if the repository already exists and is within its TTL
	if !m.needsSync(info) {
		return nil
	}

	// 2. Clone/Fetch the upstream mirror
	changed, err := m.syncMirror(info)
	if err != nil {
		return err
	}

	// If the final repository already exists, and we might have returned false from syncMirror,
	// we check if we can completely bypass the conversion.
	servingHeadPath := filepath.Join(info.ServingPath, "HEAD")
	if _, err := os.Stat(servingHeadPath); err == nil {
		// If we could not fetch updates, or if nothing has changed, we should just keep the current converted serving repo.
		// Let's touch HEAD to reset the TTL so we don't spam fetch on every request.
		if !changed {
			now := time.Now()
			_ = os.Chtimes(servingHeadPath, now, now)
			return nil
		}
	}

	// 3. Convert mirror to serving repo (SHA1 -> SHA256 conversion)
	if err := m.convertRepo(info, progressWriter); err != nil {
		return err
	}

	return nil
}

func (m *Manager) needsSync(info *resolver.RepositoryInfo) bool {
	fi, err := os.Stat(filepath.Join(info.ServingPath, "HEAD"))
	if err != nil {
		return true // Serving repo doesn't exist, must clone and convert
	}
	return time.Since(fi.ModTime()) > m.cacheTTL
}

func (m *Manager) syncMirror(info *resolver.RepositoryInfo) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(info.MirrorPath), 0755); err != nil {
		return false, fmt.Errorf("failed to create mirrors directory: %w", err)
	}

	if _, err := os.Stat(info.MirrorPath); os.IsNotExist(err) {
		// Clone as a bare mirror
		_, err := runCmd("", "git", "clone", "--mirror", info.RemoteURL, info.MirrorPath)
		if err != nil {
			return false, fmt.Errorf("failed to clone remote %s: %w", info.RemoteURL, err)
		}
		if err := excludeBackupRefsFromFetch(info.MirrorPath); err != nil {
			return false, err
		}
		return true, nil // Newly cloned, definitely changed
	} else {
		// Also applied here to migrate mirrors created before the exclusion existed
		if err := excludeBackupRefsFromFetch(info.MirrorPath); err != nil {
			return false, err
		}

		// Read current refs before fetching
		oldRefsOutput, err := runCmd(info.MirrorPath, "git", "show-ref")
		var oldRefs map[string]string
		if err == nil {
			oldRefs = parseShowRef(oldRefsOutput)
		}

		// Fetch updates
		_, err = runCmd(info.MirrorPath, "git", "fetch", "--prune")
		if err != nil {
			// Check if we have an existing serving repository to fall back to
			if _, statErr := os.Stat(filepath.Join(info.ServingPath, "HEAD")); statErr == nil {
				log.Printf("WARNING: Upstream fetch failed for %s (%v). Falling back to cached repository.", info.RemoteURL, err)
				return false, nil
			}
			return false, fmt.Errorf("upstream fetch failed and no local cache available for %s: %w", info.RemoteURL, err)
		}

		// Create backup refs if there were old refs
		if len(oldRefs) > 0 {
			m.backupForcePushedRefs(info, oldRefs)
		}

		// Read new refs after fetching & backup creation
		newRefsOutput, err := runCmd(info.MirrorPath, "git", "show-ref")
		var newRefs map[string]string
		if err == nil {
			newRefs = parseShowRef(newRefsOutput)
		}

		// Compare references to determine if anything changed
		if len(oldRefs) != len(newRefs) {
			return true, nil
		}
		for ref, oldOID := range oldRefs {
			newOID, exists := newRefs[ref]
			if !exists || oldOID != newOID {
				return true, nil
			}
		}

		return false, nil // No changes detected!
	}
}

// backupRefsNegativeRefspec keeps "git fetch --prune" from deleting our backup refs.
// A --mirror clone fetches with "+refs/*:refs/*", so without it every ref absent upstream
// (including refs/evergit-backups/*) is pruned. Requires Git >= 2.29.
const backupRefsNegativeRefspec = "^refs/evergit-backups/*"

func excludeBackupRefsFromFetch(mirrorPath string) error {
	refspecs, _ := runCmd(mirrorPath, "git", "config", "--get-all", "remote.origin.fetch")
	for _, refspec := range strings.Split(refspecs, "\n") {
		if strings.TrimSpace(refspec) == backupRefsNegativeRefspec {
			return nil
		}
	}
	if _, err := runCmd(mirrorPath, "git", "config", "--add", "remote.origin.fetch", backupRefsNegativeRefspec); err != nil {
		return fmt.Errorf("failed to exclude backup refs from fetch in %s: %w", mirrorPath, err)
	}
	return nil
}

func (m *Manager) convertRepo(info *resolver.RepositoryInfo, progressWriter io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(info.ServingPath), 0755); err != nil {
		return fmt.Errorf("failed to create repos directory: %w", err)
	}

	// Paths for persistent marks files inside the repository directory
	sha1MarksPath := filepath.Join(info.ServingPath, "evergit-sha1-marks.txt")
	sha256MarksPath := filepath.Join(info.ServingPath, "evergit-sha256-marks.txt")

	// Check if we are doing incremental conversion
	isIncremental := false
	if _, err := os.Stat(filepath.Join(info.ServingPath, "HEAD")); err == nil {
		if _, err1 := os.Stat(sha1MarksPath); err1 == nil {
			if _, err2 := os.Stat(sha256MarksPath); err2 == nil {
				isIncremental = true
			}
		}
	}

	var targetPath string
	var exportFlags []string
	var importFlags []string
	var oldSha1Size, oldSha256Size int64

	if isIncremental {
		targetPath = info.ServingPath
		log.Printf("Starting high-performance incremental conversion for repository %s...", info.RepoName)
		if progressWriter != nil {
			_, _ = fmt.Fprintf(progressWriter, "remote: Evergit: Incremental update found. Performing incremental JIT conversion...\n")
		}

		if fi, err := os.Stat(sha1MarksPath); err == nil {
			oldSha1Size = fi.Size()
		}
		if fi, err := os.Stat(sha256MarksPath); err == nil {
			oldSha256Size = fi.Size()
		}

		// Temporarily unset compatObjectFormat so git fast-import does not crash on existing tree mapping checks
		_, _ = runCmd(targetPath, "git", "--git-dir=.", "config", "--unset", "extensions.compatObjectFormat")

		exportFlags = []string{"--all", "--signed-commits=strip", "--tag-of-filtered-object=rewrite", "--import-marks=" + sha1MarksPath, "--export-marks=" + sha1MarksPath}
		importFlags = []string{"--force", "--import-marks=" + sha256MarksPath, "--export-marks=" + sha256MarksPath}
	} else {
		// Full conversion
		targetPath = info.ServingPath + ".tmp"
		os.RemoveAll(targetPath)
		log.Printf("Starting full conversion for repository %s...", info.RepoName)
		if progressWriter != nil {
			_, _ = fmt.Fprintf(progressWriter, "remote: Evergit: Updates found. Performing full JIT conversion...\n")
		}

		// Init bare SHA256 repo
		_, err := runCmd("", "git", "init", "--bare", "--object-format=sha256", targetPath)
		if err != nil {
			return fmt.Errorf("failed to initialize bare sha256 repository: %w", err)
		}

		// (We will enable compatibility mapping ONLY after fast-import completes successfully)

		sha1TmpMarksPath := filepath.Join(targetPath, "evergit-sha1-marks.txt")
		sha256TmpMarksPath := filepath.Join(targetPath, "evergit-sha256-marks.txt")

		exportFlags = []string{"--all", "--signed-commits=strip", "--tag-of-filtered-object=rewrite", "--export-marks=" + sha1TmpMarksPath}
		importFlags = []string{"--export-marks=" + sha256TmpMarksPath}
	}

	// Run fast-export and fast-import
	exportCmd := exec.Command("git", append([]string{"fast-export"}, exportFlags...)...)
	exportCmd.Dir = info.MirrorPath

	importCmd := exec.Command("git", append([]string{"fast-import"}, importFlags...)...)
	importCmd.Dir = targetPath

	r, w := io.Pipe()
	exportCmd.Stdout = w
	importCmd.Stdin = r

	var exportErrBuf, importErrBuf strings.Builder
	exportCmd.Stderr = &exportErrBuf

	// Stream fast-import progress to progressWriter in real-time
	importStderrPipe, err := importCmd.StderrPipe()
	if err != nil {
		_ = r.Close()
		_ = w.Close()
		return fmt.Errorf("failed to create stderr pipe for git fast-import: %w", err)
	}

	if err := exportCmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return fmt.Errorf("failed to start git fast-export: %w", err)
	}
	if err := importCmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return fmt.Errorf("failed to start git fast-import: %w", err)
	}

	// Stream stderr of fast-import in real-time
	go func() {
		scanner := bufio.NewScanner(importStderrPipe)
		for scanner.Scan() {
			line := scanner.Text()
			log.Printf("[%s fast-import] %s", info.RepoName, line)
			if progressWriter != nil {
				_, _ = fmt.Fprintf(progressWriter, "remote: Evergit: [Progress] %s\n", line)
			}
		}
	}()

	exportErrChan := make(chan error, 1)
	go func() {
		err := exportCmd.Wait()
		_ = w.CloseWithError(err)
		exportErrChan <- err
	}()

	importErr := importCmd.Wait()
	_ = r.Close()

	exportErr := <-exportErrChan

	if exportErr != nil {
		if !isIncremental {
			os.RemoveAll(targetPath)
		}
		return fmt.Errorf("git fast-export failed: %v (stderr: %q)", exportErr, strings.TrimSpace(exportErrBuf.String()))
	}
	if importErr != nil {
		if !isIncremental {
			os.RemoveAll(targetPath)
		}
		return fmt.Errorf("git fast-import failed: %v (stderr: %q)", importErr, strings.TrimSpace(importErrBuf.String()))
	}

	// Enable/Restore SHA1 compatibility mapping on the repository
	_, err = runCmd(targetPath, "git", "--git-dir=.", "config", "extensions.compatObjectFormat", "sha1")
	if err != nil {
		log.Printf("WARNING: failed to configure extensions.compatObjectFormat in %s: %v", targetPath, err)
	}

	// Get HEAD symbolic-ref from mirror
	headRef, err := runCmd(info.MirrorPath, "git", "symbolic-ref", "HEAD")
	if err != nil {
		headRef = "refs/heads/main"
	}

	// Generate loose-object-idx bidirectional hash translation map
	var genErr error
	if isIncremental {
		genErr = generateLooseObjectIdxIncremental(sha1MarksPath, sha256MarksPath, targetPath, oldSha1Size, oldSha256Size)
	} else {
		sha1TmpMarksPath := filepath.Join(targetPath, "evergit-sha1-marks.txt")
		sha256TmpMarksPath := filepath.Join(targetPath, "evergit-sha256-marks.txt")
		genErr = generateLooseObjectIdx(sha1TmpMarksPath, sha256TmpMarksPath, targetPath)
	}
	if genErr != nil {
		log.Printf("WARNING: failed to generate loose-object-idx compatibility map: %v", genErr)
	}

	// Set the default branch (HEAD) symbolic ref
	_, err = runCmd(targetPath, "git", "--git-dir=.", "symbolic-ref", "HEAD", headRef)
	if err != nil {
		log.Printf("WARNING: failed to set symbolic-ref HEAD in %s to %s: %v", targetPath, headRef, err)
	}

	// Update the mtime of the HEAD file to represent our sync/cache point
	now := time.Now()
	_ = os.Chtimes(filepath.Join(targetPath, "HEAD"), now, now)

	if !isIncremental {
		// Swap atomically only for full conversion
		os.RemoveAll(info.ServingPath)
		if err := os.Rename(targetPath, info.ServingPath); err != nil {
			return fmt.Errorf("failed to atomically swap converted repository to %s: %w", info.ServingPath, err)
		}
	}

	// Log the translated backup refs to show both SHA256 and SHA1 mappings
	m.logBackupRefs(info)

	return nil
}

func runCmd(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("cmd %s %v failed in %s: %w (stderr: %q)", name, args, dir, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func generateLooseObjectIdx(sha1MarksPath, sha256MarksPath, repoPath string) error {
	sha1Marks, err := parseMarksFile(sha1MarksPath)
	if err != nil {
		return fmt.Errorf("failed to parse SHA1 marks: %w", err)
	}

	sha256Marks, err := parseMarksFile(sha256MarksPath)
	if err != nil {
		return fmt.Errorf("failed to parse SHA256 marks: %w", err)
	}

	// Build the mapping content
	var sb strings.Builder
	sb.WriteString("# loose-object-idx\n")

	for mark, sha1Hash := range sha1Marks {
		sha256Hash, ok := sha256Marks[mark]
		if ok {
			sb.WriteString(fmt.Sprintf("%s %s\n", sha256Hash, sha1Hash))
		}
	}

	idxPath := filepath.Join(repoPath, "objects", "loose-object-idx")
	if err := os.WriteFile(idxPath, []byte(sb.String()), 0644); err != nil {
		return fmt.Errorf("failed to write loose-object-idx file: %w", err)
	}

	return nil
}

func parseMarksFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	marks := make(map[string]string)
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) == 2 {
			marks[parts[0]] = parts[1]
		}
	}
	return marks, nil
}

func (m *Manager) backupForcePushedRefs(info *resolver.RepositoryInfo, oldRefs map[string]string) {
	// List new refs
	newRefsOutput, err := runCmd(info.MirrorPath, "git", "show-ref")
	if err != nil {
		newRefsOutput = ""
	}

	newRefs := parseShowRef(newRefsOutput)
	nowStr := time.Now().Format("20060102-150405")

	for ref, oldOID := range oldRefs {
		if strings.HasPrefix(ref, "refs/evergit-backups/") {
			continue
		}
		if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
			continue
		}

		newOID, exists := newRefs[ref]
		if exists && oldOID == newOID {
			continue
		}
		// A fast-forwarded branch still reaches the old commit, so there is nothing to archive.
		// Tags are always archived on change, since the old tag object itself would be lost.
		if exists && strings.HasPrefix(ref, "refs/heads/") {
			if _, err := runCmd(info.MirrorPath, "git", "merge-base", "--is-ancestor", oldOID, newOID); err == nil {
				continue
			}
		}
		// The ref was deleted or overwritten (force pushed)
		shortSHA := oldOID
		if len(shortSHA) > 8 {
			shortSHA = shortSHA[:8]
		}
		backupRef := fmt.Sprintf("refs/evergit-backups/%s/%s-%s", strings.TrimPrefix(ref, "refs/"), nowStr, shortSHA)

		log.Printf("[Backup] Creating backup reference %s pointing to old commit %s", backupRef, oldOID)
		_, err := runCmd(info.MirrorPath, "git", "update-ref", backupRef, oldOID)
		if err != nil {
			log.Printf("WARNING: failed to create backup ref %s pointing to %s: %v", backupRef, oldOID, err)
		}
	}
}

func parseShowRef(output string) map[string]string {
	refs := make(map[string]string)
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) == 2 {
			oid := parts[0]
			ref := parts[1]
			refs[ref] = oid
		}
	}
	return refs
}

func (m *Manager) logBackupRefs(info *resolver.RepositoryInfo) {
	// List all refs in the serving repo
	showRefOut, err := runCmd(info.ServingPath, "git", "--git-dir=.", "show-ref")
	if err != nil {
		return
	}

	// Parse loose-object-idx to map sha256 -> sha1
	idxPath := filepath.Join(info.ServingPath, "objects", "loose-object-idx")
	idxData, err := os.ReadFile(idxPath)
	if err != nil {
		return
	}

	sha256ToSha1 := make(map[string]string)
	lines := strings.Split(string(idxData), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) == 2 {
			sha256ToSha1[parts[0]] = parts[1]
		}
	}

	// Print mapping for each backup ref
	refLines := strings.Split(showRefOut, "\n")
	for _, refLine := range refLines {
		refLine = strings.TrimSpace(refLine)
		if refLine == "" {
			continue
		}
		parts := strings.SplitN(refLine, " ", 2)
		if len(parts) == 2 {
			sha256 := parts[0]
			refName := parts[1]

			if strings.HasPrefix(refName, "refs/evergit-backups/") {
				sha1, ok := sha256ToSha1[sha256]
				if !ok {
					sha1 = "unknown"
				}
				log.Printf("[Backup] Preserved history: %s -> SHA256: %s (SHA1: %s)", refName, sha256, sha1)
			}
		}
	}
}

func generateLooseObjectIdxIncremental(sha1MarksPath, sha256MarksPath, repoPath string, oldSha1Size, oldSha256Size int64) error {
	sha1Marks, err := parseNewMarks(sha1MarksPath, oldSha1Size)
	if err != nil {
		return err
	}

	sha256Marks, err := parseNewMarks(sha256MarksPath, oldSha256Size)
	if err != nil {
		return err
	}

	if len(sha1Marks) == 0 || len(sha256Marks) == 0 {
		return nil // No new objects to map
	}

	// Append new mappings to loose-object-idx
	idxPath := filepath.Join(repoPath, "objects", "loose-object-idx")
	f, err := os.OpenFile(idxPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open loose-object-idx for appending: %w", err)
	}
	defer f.Close()

	for mark, sha1Hash := range sha1Marks {
		sha256Hash, ok := sha256Marks[mark]
		if ok {
			if _, err := f.WriteString(fmt.Sprintf("%s %s\n", sha256Hash, sha1Hash)); err != nil {
				return fmt.Errorf("failed to append to loose-object-idx: %w", err)
			}
		}
	}

	return nil
}

func parseNewMarks(path string, startOffset int64) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	_, err = f.Seek(startOffset, io.SeekStart)
	if err != nil {
		return nil, err
	}

	marks := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) == 2 {
			marks[parts[0]] = parts[1]
		}
	}

	return marks, scanner.Err()
}
