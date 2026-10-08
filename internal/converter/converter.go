package converter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"evergit/internal/resolver"
)

type Manager struct {
	cacheTTL  time.Duration
	locksMu   sync.Mutex
	repoLocks map[string]*repoLock
}

// repoLock is a per-repository mutex, reference-counted so idle entries can be dropped.
type repoLock struct {
	mu    sync.Mutex
	users int
}

func NewManager(cacheTTL time.Duration) *Manager {
	return &Manager{
		cacheTTL:  cacheTTL,
		repoLocks: make(map[string]*repoLock),
	}
}

// EnsureRepo checks if the repository exists, is up-to-date, and is converted.
// If not, it clones/fetches, converts it, and optionally streams progress.
func (m *Manager) EnsureRepo(info *resolver.RepositoryInfo, progressWriter io.Writer) error {
	if progressWriter != nil {
		async := newAsyncWriter(progressWriter)
		defer async.Close()
		progressWriter = async
	}

	unlock := m.lockRepo(info.ServingPath)
	defer unlock()

	// 1. Check if the repository already exists and is within its TTL
	if !m.needsSync(info) {
		return nil
	}

	// 2. Clone/Fetch the upstream mirror
	changed, err := m.syncMirror(info)
	if err != nil {
		return err
	}

	// Skip conversion when the serving repo exists and the mirror did not change (or the fetch
	// failed and we fall back to the cache). Touching HEAD restarts the TTL.
	servingHeadPath := filepath.Join(info.ServingPath, "HEAD")
	if _, err := os.Stat(servingHeadPath); err == nil {
		if !changed {
			if err := ensureLooseObjectIdx(info.ServingPath); err != nil {
				return err
			}
			// Repairs the setting if a previous conversion was killed mid-import
			if err := enableCompatObjectFormat(info.ServingPath); err != nil {
				return err
			}
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

// lockRepo acquires the repository's lock and returns its release function. Entries are removed
// once unused, so the map does not grow with every distinct path ever requested.
func (m *Manager) lockRepo(key string) func() {
	m.locksMu.Lock()
	lock, exists := m.repoLocks[key]
	if !exists {
		lock = &repoLock{}
		m.repoLocks[key] = lock
	}
	lock.users++
	m.locksMu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		m.locksMu.Lock()
		if lock.users--; lock.users == 0 {
			delete(m.repoLocks, key)
		}
		m.locksMu.Unlock()
	}
}

// asyncWriter forwards writes from a goroutine and drops them when its buffer is full, so a
// client that stops reading progress cannot stall a conversion holding the repository lock.
type asyncWriter struct {
	lines chan []byte
	done  chan struct{}
}

func newAsyncWriter(w io.Writer) *asyncWriter {
	a := &asyncWriter{lines: make(chan []byte, 256), done: make(chan struct{})}
	go func() {
		defer close(a.done)
		for line := range a.lines {
			_, _ = w.Write(line)
		}
	}()
	return a
}

func (a *asyncWriter) Write(p []byte) (int, error) {
	select {
	case a.lines <- append([]byte(nil), p...):
	default: // Reader is not keeping up; drop the progress line
	}
	return len(p), nil
}

// Close stops accepting writes and gives queued lines a moment to flush, so progress is not
// interleaved with output written after the conversion. A stalled reader is abandoned.
func (a *asyncWriter) Close() {
	close(a.lines)
	select {
	case <-a.done:
	case <-time.After(time.Second):
	}
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

	_, statErr := os.Stat(info.MirrorPath)
	if statErr == nil && !looksLikeBareRepo(info.MirrorPath) {
		// git would otherwise walk up from this directory and operate on an enclosing repository
		log.Printf("WARNING: mirror %s is not a git repository (interrupted clone?), re-cloning", info.MirrorPath)
		if err := os.RemoveAll(info.MirrorPath); err != nil {
			return false, fmt.Errorf("failed to remove broken mirror %s: %w", info.MirrorPath, err)
		}
		statErr = os.ErrNotExist
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		return false, fmt.Errorf("failed to access mirror %s: %w", info.MirrorPath, statErr)
	}

	if os.IsNotExist(statErr) {
		// Clone as a bare mirror
		// --bare rather than --mirror: only branches and tags, not forge refs such as GitHub's
		// thousands of refs/pull/* from forks
		_, err := runCmdTimeout(upstreamTimeout, "", "git", "clone", "--bare", info.RemoteURL, info.MirrorPath)
		if err != nil {
			return false, fmt.Errorf("failed to clone remote %s: %w", info.RemoteURL, err)
		}
		if err := configureMirrorRefs(info.MirrorPath); err != nil {
			return false, err
		}
		return true, nil // Newly cloned, definitely changed
	} else {
		// Also applied here to migrate mirrors created by earlier versions with "clone --mirror"
		if err := configureMirrorRefs(info.MirrorPath); err != nil {
			return false, err
		}

		// Read current refs before fetching: without this baseline, force-pushes cannot be archived
		oldRefs, err := listRefs(info.MirrorPath)
		if err != nil {
			if _, statErr := os.Stat(filepath.Join(info.ServingPath, "HEAD")); statErr == nil {
				log.Printf("WARNING: cannot read refs of mirror %s (%v), skipping upstream fetch and serving cached repository", info.MirrorPath, err)
				return false, nil
			}
			return false, fmt.Errorf("failed to read refs of mirror %s: %w", info.MirrorPath, err)
		}

		// Fetch updates
		_, err = runCmdTimeout(upstreamTimeout, info.MirrorPath, "git", "fetch", "--prune")

		// Archive overwritten/deleted refs even if the fetch failed midway, since it may already
		// have pruned or updated some of them
		if len(oldRefs) > 0 {
			m.backupForcePushedRefs(info, oldRefs)
		}

		if err != nil {
			// Check if we have an existing serving repository to fall back to
			if _, statErr := os.Stat(filepath.Join(info.ServingPath, "HEAD")); statErr == nil {
				log.Printf("WARNING: Upstream fetch failed for %s (%v). Falling back to cached repository.", info.RemoteURL, err)
				return false, nil
			}
			return false, fmt.Errorf("upstream fetch failed and no local cache available for %s: %w", info.RemoteURL, err)
		}

		// Read new refs after fetching & backup creation
		newRefs, err := listRefs(info.MirrorPath)
		if err != nil {
			return false, fmt.Errorf("failed to read refs of mirror %s: %w", info.MirrorPath, err)
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

// looksLikeBareRepo is a deliberately structural check, so a transient git failure can never
// cause a valid mirror (and its backup refs) to be deleted.
func looksLikeBareRepo(path string) bool {
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil {
		return false
	}
	fi, err := os.Stat(filepath.Join(path, "objects"))
	return err == nil && fi.IsDir()
}

// mirrorRefspecs restrict the mirror to branches and tags. Forge refs (refs/pull/*,
// refs/merge-requests/*, ...) are not needed and can number in the tens of thousands. Being
// scoped to heads and tags, "git fetch --prune" can never delete refs/evergit-backups/*.
var mirrorRefspecs = []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}

// configureMirrorRefs sets the mirror's fetch refspecs and, for mirrors created by earlier
// versions with "clone --mirror", deletes the refs outside of them once.
func configureMirrorRefs(mirrorPath string) error {
	current, _ := runCmd(mirrorPath, "git", "config", "--get-all", "remote.origin.fetch")
	if current == strings.Join(mirrorRefspecs, "\n") {
		return nil
	}

	if _, err := runCmd(mirrorPath, "git", "config", "--replace-all", "remote.origin.fetch", mirrorRefspecs[0]); err != nil {
		return fmt.Errorf("failed to configure fetch refspecs in %s: %w", mirrorPath, err)
	}
	for _, refspec := range mirrorRefspecs[1:] {
		if _, err := runCmd(mirrorPath, "git", "config", "--add", "remote.origin.fetch", refspec); err != nil {
			return fmt.Errorf("failed to configure fetch refspecs in %s: %w", mirrorPath, err)
		}
	}

	refs, err := listRefs(mirrorPath)
	if err != nil {
		return err
	}
	var deletions strings.Builder
	for ref := range refs {
		if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") && !strings.HasPrefix(ref, "refs/evergit-backups/") {
			fmt.Fprintf(&deletions, "delete %s\n", ref)
		}
	}
	if deletions.Len() == 0 {
		return nil
	}
	log.Printf("Migrating mirror %s to branches and tags only", mirrorPath)
	cmd := exec.Command("git", "update-ref", "--stdin")
	cmd.Dir = mirrorPath
	cmd.Env = GitEnv()
	cmd.Stdin = strings.NewReader(deletions.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to remove non-branch refs from %s: %w (output: %q)", mirrorPath, err, strings.TrimSpace(string(out)))
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

	if isIncremental {
		targetPath = info.ServingPath
		log.Printf("Starting high-performance incremental conversion for repository %s...", info.RepoName)
		if progressWriter != nil {
			_, _ = fmt.Fprintf(progressWriter, "remote: Evergit: Incremental update found. Performing incremental JIT conversion...\n")
		}

		// Temporarily unset compatObjectFormat so git fast-import does not crash on existing tree mapping checks
		_, _ = runCmd(targetPath, "git", "--git-dir=.", "config", "--unset", "extensions.compatObjectFormat")
		defer func() {
			if err := enableCompatObjectFormat(targetPath); err != nil {
				log.Printf("WARNING: %v", err)
			}
		}()

		exportFlags = []string{"--all", "--signed-commits=strip", "--tag-of-filtered-object=rewrite", "--import-marks=" + sha1MarksPath, "--export-marks=" + sha1MarksPath}
		importFlags = []string{"--force", "--import-marks=" + sha256MarksPath, "--export-marks=" + sha256MarksPath}
	} else {
		// Full conversion into a fresh build directory, published by publishBuild once complete
		targetPath = filepath.Join(info.BuildsPath, strconv.FormatInt(time.Now().UnixNano(), 10))
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
	exportCmd.Env = GitEnv()
	exportCmd.Dir = info.MirrorPath

	importCmd := exec.Command("git", append([]string{"fast-import"}, importFlags...)...)
	importCmd.Env = GitEnv()
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
		_ = exportCmd.Process.Kill()
		_ = exportCmd.Wait()
		return fmt.Errorf("failed to start git fast-import: %w", err)
	}

	// Stream stderr of fast-import in real-time, keeping it for error reporting
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(importStderrPipe)
		for scanner.Scan() {
			line := scanner.Text()
			importErrBuf.WriteString(line + "\n")
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

	// All stderr reads must complete before Wait closes the pipe
	<-stderrDone
	importErr := importCmd.Wait()
	_ = r.Close()

	exportErr := <-exportErrChan

	// Check fast-import first: when it fails, fast-export usually fails too, on a broken pipe
	if importErr != nil {
		if !isIncremental {
			os.RemoveAll(targetPath)
		}
		return fmt.Errorf("git fast-import failed: %v (stderr: %q)", importErr, strings.TrimSpace(importErrBuf.String()))
	}
	if exportErr != nil {
		if !isIncremental {
			os.RemoveAll(targetPath)
		}
		return fmt.Errorf("git fast-export failed: %v (stderr: %q)", exportErr, strings.TrimSpace(exportErrBuf.String()))
	}

	// fast-import never deletes refs, so drop the ones that no longer exist in the mirror
	if isIncremental {
		if err := pruneServingRefs(info); err != nil {
			return err
		}
	}

	// Enable SHA1 compatibility mapping on the new repository (restored by the deferred call when incremental)
	if !isIncremental {
		if err := enableCompatObjectFormat(targetPath); err != nil {
			log.Printf("WARNING: %v", err)
		}
	}

	// Get HEAD symbolic-ref from mirror
	headRef, err := runCmd(info.MirrorPath, "git", "symbolic-ref", "HEAD")
	if err != nil {
		headRef = "refs/heads/main"
	}

	// Regenerate the loose-object-idx translation map from the complete marks files
	if err := generateLooseObjectIdx(targetPath); err != nil {
		if !isIncremental {
			os.RemoveAll(targetPath)
		}
		return fmt.Errorf("failed to generate loose-object-idx compatibility map: %w", err)
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
		if err := publishBuild(info, targetPath); err != nil {
			os.RemoveAll(targetPath)
			return err
		}
	}

	// Log the translated backup refs to show both SHA256 and SHA1 mappings
	m.logBackupRefs(info)

	return nil
}

// publishBuild atomically points the ServingPath symlink at buildPath, so readers always see
// either the old or the new repository. The previous build is kept for readers still using it;
// older builds are removed.
func publishBuild(info *resolver.RepositoryInfo, buildPath string) error {
	previous := ""
	if fi, err := os.Lstat(info.ServingPath); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(info.ServingPath); err == nil {
				previous = filepath.Join(filepath.Dir(info.ServingPath), target)
			}
		} else {
			// Serving repo created before builds existed: move it aside so the symlink can replace it
			previous = filepath.Join(info.BuildsPath, "legacy")
			os.RemoveAll(previous)
			if err := os.Rename(info.ServingPath, previous); err != nil {
				return fmt.Errorf("failed to move legacy serving repository %s aside: %w", info.ServingPath, err)
			}
		}
	}

	target, err := filepath.Rel(filepath.Dir(info.ServingPath), buildPath)
	if err != nil {
		return fmt.Errorf("failed to compute build symlink target: %w", err)
	}
	tmpLink := info.ServingPath + ".link"
	os.Remove(tmpLink)
	if err := os.Symlink(target, tmpLink); err != nil {
		return fmt.Errorf("failed to create build symlink: %w", err)
	}
	if err := os.Rename(tmpLink, info.ServingPath); err != nil {
		os.Remove(tmpLink)
		return fmt.Errorf("failed to publish build %s to %s: %w", buildPath, info.ServingPath, err)
	}

	// Leftover from versions that built next to the serving repo
	os.RemoveAll(info.ServingPath + ".tmp")
	builds, err := os.ReadDir(info.BuildsPath)
	if err != nil {
		return nil
	}
	for _, build := range builds {
		path := filepath.Join(info.BuildsPath, build.Name())
		if path != buildPath && path != previous {
			os.RemoveAll(path)
		}
	}
	return nil
}

// enableCompatObjectFormat sets extensions.compatObjectFormat=sha1 unless it is already set.
func enableCompatObjectFormat(repoPath string) error {
	if format, err := runCmd(repoPath, "git", "--git-dir=.", "config", "extensions.compatObjectFormat"); err == nil && format == "sha1" {
		return nil
	}
	if _, err := runCmd(repoPath, "git", "--git-dir=.", "config", "extensions.compatObjectFormat", "sha1"); err != nil {
		return fmt.Errorf("failed to configure extensions.compatObjectFormat in %s: %w", repoPath, err)
	}
	return nil
}

// pruneServingRefs deletes serving refs that are absent from the mirror, e.g. branches deleted
// upstream (their history is archived under refs/evergit-backups/ by backupForcePushedRefs).
// Backup refs are never pruned: older versions lost some from the mirror, leaving the serving
// repo as their only copy.
func pruneServingRefs(info *resolver.RepositoryInfo) error {
	mirrorRefs, err := runCmd(info.MirrorPath, "git", "for-each-ref", "--format=%(refname)")
	if err != nil {
		return err
	}
	servingRefs, err := runCmd(info.ServingPath, "git", "--git-dir=.", "for-each-ref", "--format=%(refname)")
	if err != nil {
		return err
	}

	inMirror := make(map[string]bool)
	for _, ref := range strings.Split(mirrorRefs, "\n") {
		inMirror[ref] = true
	}
	var deletions strings.Builder
	for _, ref := range strings.Split(servingRefs, "\n") {
		if ref == "" || inMirror[ref] || strings.HasPrefix(ref, "refs/evergit-backups/") {
			continue
		}
		log.Printf("[Prune] Removing ref %s, no longer present upstream", ref)
		fmt.Fprintf(&deletions, "delete %s\n", ref)
	}
	if deletions.Len() == 0 {
		return nil
	}

	cmd := exec.Command("git", "--git-dir=.", "update-ref", "--stdin")
	cmd.Env = GitEnv()
	cmd.Dir = info.ServingPath
	cmd.Stdin = strings.NewReader(deletions.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to prune refs in %s: %w (output: %q)", info.ServingPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// upstreamTimeout caps a whole clone or fetch; stalled transfers are aborted much earlier by the
// low-speed limit set in runCmdTimeout.
const upstreamTimeout = time.Hour

// redirectingGitEnv are variables that make git operate on a different repository, object store or
// work tree than the one Evergit chose. Inherited from the daemon's environment, they would silently
// redirect every operation, including writes.
var redirectingGitEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE",
	"GIT_QUARANTINE_PATH", "GIT_PREFIX", "GIT_DEFAULT_HASH",
}

// GitEnv returns the process environment without redirecting git variables, plus extra.
func GitEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(redirectingGitEnv, name) {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

func runCmd(dir, name string, args ...string) (string, error) {
	return runCmdTimeout(0, dir, name, args...)
}

// runCmdTimeout runs a command, killing it after timeout (0 means no limit).
func runCmdTimeout(timeout time.Duration, dir, name string, args ...string) (string, error) {
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	// Child processes (e.g. git-remote-https) may keep the output pipes open after a kill
	cmd.WaitDelay = 10 * time.Second
	cmd.Dir = dir
	cmd.Env = GitEnv("GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true",
		// Abort transfers slower than 1 KiB/s for 60s, so a hung upstream cannot hold the repo lock
		"GIT_HTTP_LOW_SPEED_LIMIT=1024", "GIT_HTTP_LOW_SPEED_TIME=60")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("cmd %s %v failed in %s: %w (stderr: %q)", name, args, dir, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// generateLooseObjectIdx rebuilds objects/loose-object-idx by pairing the SHA1 and SHA256 marks files.
// Both files always hold every mark (fast-export/fast-import rewrite them in full), so the map is
// rebuilt from scratch rather than appended to, and swapped in atomically for concurrent readers.
// Only the SHA1 marks are held in memory (fast-export writes them unordered); the SHA256 marks are
// streamed in mark order straight to the output. Malformed lines, e.g. truncated by a crash, are skipped.
func generateLooseObjectIdx(repoPath string) error {
	sha1Marks, err := parseMarksFile(filepath.Join(repoPath, "evergit-sha1-marks.txt"), 40)
	if err != nil {
		return fmt.Errorf("failed to parse SHA1 marks: %w", err)
	}

	sha256File, err := os.Open(filepath.Join(repoPath, "evergit-sha256-marks.txt"))
	if err != nil {
		return fmt.Errorf("failed to open SHA256 marks: %w", err)
	}
	defer sha256File.Close()

	idxPath := filepath.Join(repoPath, "objects", "loose-object-idx")
	tmpPath := idxPath + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("failed to create loose-object-idx file: %w", err)
	}
	defer os.Remove(tmpPath) // No-op once renamed

	w := bufio.NewWriter(out)
	_, _ = w.WriteString("# loose-object-idx\n")
	scanner := bufio.NewScanner(sha256File)
	for scanner.Scan() {
		mark, sha256Hash, ok := parseMarkLine(scanner.Text(), 64)
		if !ok {
			continue
		}
		if sha1Hash, ok := sha1Marks[mark]; ok {
			_, _ = fmt.Fprintf(w, "%s %s\n", sha256Hash, sha1Hash)
		}
	}
	if err := scanner.Err(); err != nil {
		out.Close()
		return fmt.Errorf("failed to read SHA256 marks: %w", err)
	}
	if err := w.Flush(); err != nil {
		out.Close()
		return fmt.Errorf("failed to write loose-object-idx file: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("failed to write loose-object-idx file: %w", err)
	}
	if err := os.Rename(tmpPath, idxPath); err != nil {
		return fmt.Errorf("failed to install loose-object-idx file: %w", err)
	}

	return nil
}

// ensureLooseObjectIdx regenerates the translation map if it is missing or older than the marks,
// e.g. after a failed or interrupted conversion.
func ensureLooseObjectIdx(repoPath string) error {
	marks, err := os.Stat(filepath.Join(repoPath, "evergit-sha256-marks.txt"))
	if err != nil {
		return nil // No marks to build from
	}
	idx, err := os.Stat(filepath.Join(repoPath, "objects", "loose-object-idx"))
	if err == nil && !idx.ModTime().Before(marks.ModTime()) {
		return nil
	}
	if err := generateLooseObjectIdx(repoPath); err != nil {
		return fmt.Errorf("failed to regenerate loose-object-idx compatibility map: %w", err)
	}
	return nil
}

// parseMarksFile reads a marks file into a mark -> hash map, skipping malformed lines.
func parseMarksFile(path string, hashLen int) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	marks := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if mark, hash, ok := parseMarkLine(scanner.Text(), hashLen); ok {
			marks[mark] = hash
		}
	}
	return marks, scanner.Err()
}

// parseMarkLine parses ":<mark> <hash>", requiring hash to be hashLen lowercase hex digits.
func parseMarkLine(line string, hashLen int) (mark, hash string, ok bool) {
	mark, hash, found := strings.Cut(strings.TrimSpace(line), " ")
	if !found || len(mark) < 2 || mark[0] != ':' || len(hash) != hashLen || strings.Trim(hash, "0123456789abcdef") != "" {
		return "", "", false
	}
	return mark, hash, true
}
func (m *Manager) backupForcePushedRefs(info *resolver.RepositoryInfo, oldRefs map[string]string) {
	newRefs, err := listRefs(info.MirrorPath)
	if err != nil {
		// Without the current refs every ref would look deleted
		log.Printf("WARNING: cannot read refs of mirror %s, skipping backups: %v", info.MirrorPath, err)
		return
	}
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

// listRefs returns refname -> object id for all refs. Unlike show-ref, for-each-ref succeeds on a
// repository without refs, so an error always means the refs could not be read.
func listRefs(repoPath string) (map[string]string, error) {
	out, err := runCmd(repoPath, "git", "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return nil, err
	}
	return parseShowRef(out), nil
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
	backupRefsOut, err := runCmd(info.ServingPath, "git", "--git-dir=.", "for-each-ref", "--format=%(objectname) %(refname)", "refs/evergit-backups/")
	if err != nil || backupRefsOut == "" {
		return
	}
	backups := parseShowRef(backupRefsOut) // refname -> sha256

	// One pass over the translation map, keeping only the backup targets
	sha256ToSha1 := make(map[string]string)
	for _, sha256 := range backups {
		sha256ToSha1[sha256] = "unknown"
	}
	if f, err := os.Open(filepath.Join(info.ServingPath, "objects", "loose-object-idx")); err == nil {
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			sha256, sha1, found := strings.Cut(scanner.Text(), " ")
			if _, wanted := sha256ToSha1[sha256]; found && wanted {
				sha256ToSha1[sha256] = sha1
			}
		}
		f.Close()
	}

	for refName, sha256 := range backups {
		log.Printf("[Backup] Preserved history: %s -> SHA256: %s (SHA1: %s)", refName, sha256, sha256ToSha1[sha256])
	}
}

var (
	ErrIDInvalid   = errors.New("invalid object id")
	ErrIDNotFound  = errors.New("object id not found")
	ErrIDAmbiguous = errors.New("ambiguous object id prefix")
)

// minIDPrefix is the shortest abbreviated object id accepted, matching git's default abbreviation.
const minIDPrefix = 7

// LookupID translates a full or abbreviated object id between hash formats using the repository's
// loose-object-idx. format is the format of id ("sha1" or "sha256"); the id in the other format is
// returned.
func LookupID(repoPath, format, id string) (string, error) {
	var column, maxLen int
	switch format {
	case "sha1":
		column, maxLen = 1, 40
	case "sha256":
		column, maxLen = 0, 64
	default:
		return "", fmt.Errorf("%w: unknown hash format %q", ErrIDInvalid, format)
	}
	id = strings.ToLower(id)
	if len(id) < minIDPrefix || len(id) > maxLen || strings.Trim(id, "0123456789abcdef") != "" {
		return "", fmt.Errorf("%w: %q is not a %s id of %d to %d hex digits", ErrIDInvalid, id, format, minIDPrefix, maxLen)
	}

	f, err := os.Open(filepath.Join(repoPath, "objects", "loose-object-idx"))
	if err != nil {
		return "", fmt.Errorf("failed to open loose-object-idx: %w", err)
	}
	defer f.Close()

	match := ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || strings.HasPrefix(fields[0], "#") || !strings.HasPrefix(fields[column], id) {
			continue
		}
		other := fields[1-column]
		if match != "" && match != other {
			return "", fmt.Errorf("%w: %s", ErrIDAmbiguous, id)
		}
		match = other
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("failed to read loose-object-idx: %w", err)
	}
	if match == "" {
		return "", fmt.Errorf("%w: %s", ErrIDNotFound, id)
	}
	return match, nil
}
