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
	"regexp"
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

	// If the upstream mirror is natively SHA-256, bypass the fast-export | fast-import pipeline
	if isRepoSHA256(info.MirrorPath) {
		return m.syncNativeSHA256(info, changed, progressWriter)
	}

	// Skip conversion when the serving repo exists and the mirror did not change (or the fetch
	// failed and we fall back to the cache). Touching HEAD restarts the TTL.
	servingHeadPath := filepath.Join(info.ServingPath, "HEAD")
	if _, err := os.Stat(servingHeadPath); err == nil {
		if !changed {
			// loose object idx healing if daemon is killed
			if err := ensureLooseObjectIdx(info.ServingPath); err != nil {
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

// isRepoSHA256 checks if a git repository is configured with the sha256 object format.
func isRepoSHA256(repoPath string) bool {
	format, err := runCmd(repoPath, "git", "--git-dir=.", "rev-parse", "--show-object-format")
	return err == nil && strings.TrimSpace(format) == "sha256"
}

// syncNativeSHA256 creates or updates the serving repository for an upstream repository that is
// already in SHA-256 format, bypassing the CPU- and memory-intensive fast-export | fast-import pipeline.
func (m *Manager) syncNativeSHA256(info *resolver.RepositoryInfo, changed bool, progressWriter io.Writer) error {
	servingHeadPath := filepath.Join(info.ServingPath, "HEAD")
	if _, err := os.Stat(servingHeadPath); err == nil {
		if !changed {
			now := time.Now()
			_ = os.Chtimes(servingHeadPath, now, now)
			return nil
		}

		log.Printf("Syncing native SHA256 updates for repository %s...", info.RepoName)
		if progressWriter != nil {
			_, _ = fmt.Fprintf(progressWriter, "remote: Evergit: Syncing native SHA-256 updates...\n")
		}
		// Fetch branches and tags, pruning any deleted upstream branches/tags
		if _, err := runCmd(info.ServingPath, "git", "fetch", "--prune", "--", info.MirrorPath,
			"+refs/heads/*:refs/heads/*",
			"+refs/tags/*:refs/tags/*",
		); err != nil {
			return fmt.Errorf("failed to fetch native sha256 updates into %s: %w", info.ServingPath, err)
		}

		// Backup refs are never pruned: older versions or re-cloned mirrors may lose them,
		// leaving the serving repo as their only copy. Fetch them additively without --prune.
		if _, err := runCmd(info.ServingPath, "git", "fetch", "--", info.MirrorPath,
			"+refs/evergit-backups/*:refs/evergit-backups/*",
		); err != nil {
			log.Printf("WARNING: failed to fetch backup refs for native sha256 repo %s: %v", info.RepoName, err)
		}

		if headRef, err := runCmd(info.MirrorPath, "git", "symbolic-ref", "HEAD"); err == nil {
			_, _ = runCmd(info.ServingPath, "git", "--git-dir=.", "symbolic-ref", "HEAD", headRef)
		}

		now := time.Now()
		_ = os.Chtimes(servingHeadPath, now, now)
		m.logBackupRefs(info)
		return nil
	}

	// Serving repo does not exist: clone directly from the mirror into a new build
	if err := os.MkdirAll(filepath.Dir(info.ServingPath), 0755); err != nil {
		return fmt.Errorf("failed to create repos directory: %w", err)
	}

	buildPath := filepath.Join(info.BuildsPath, strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.MkdirAll(filepath.Dir(buildPath), 0755); err != nil {
		return fmt.Errorf("failed to create builds directory: %w", err)
	}

	log.Printf("Creating native SHA256 serving repository for %s...", info.RepoName)
	if progressWriter != nil {
		_, _ = fmt.Fprintf(progressWriter, "remote: Evergit: Upstream is native SHA-256. Bypassing conversion pipeline...\n")
	}
	if _, err := runCmd("", "git", "clone", "--bare", "--", info.MirrorPath, buildPath); err != nil {
		os.RemoveAll(buildPath)
		return fmt.Errorf("failed to clone native sha256 repository %s: %w", info.RepoName, err)
	}

	// clone --bare only copies heads and tags; also copy any existing backup refs
	_, _ = runCmd(buildPath, "git", "fetch", "--", info.MirrorPath, "+refs/evergit-backups/*:refs/evergit-backups/*")

	if headRef, err := runCmd(info.MirrorPath, "git", "symbolic-ref", "HEAD"); err == nil {
		_, _ = runCmd(buildPath, "git", "--git-dir=.", "symbolic-ref", "HEAD", headRef)
	}

	now := time.Now()
	_ = os.Chtimes(filepath.Join(buildPath, "HEAD"), now, now)

	if err := publishBuild(info, buildPath); err != nil {
		os.RemoveAll(buildPath)
		return err
	}

	m.logBackupRefs(info)
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
		_, err := runCmdTimeout(upstreamTimeout, "", "git", "clone", "--bare", "--", info.RemoteURL, info.MirrorPath)
		if err != nil {
			return false, fmt.Errorf("failed to clone remote %s: %w", info.RemoteURL, err)
		}
		return true, nil // Newly cloned, definitely changed
	} else {
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
		fetchArgs := append([]string{"fetch", "--prune", "origin"}, mirrorRefspecs...)
		_, err = runCmdTimeout(upstreamTimeout, info.MirrorPath, "git", fetchArgs...)

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

// mirrorRefspecs restrict mirror fetches to branches and tags. Forge refs (refs/pull/*,
// refs/merge-requests/*, ...) are not needed and can number in the tens of thousands. Being
// scoped to heads and tags, "git fetch --prune" can never delete refs/evergit-backups/*.
var mirrorRefspecs = []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}

type conversionPlan struct {
	targetPath    string
	isIncremental bool
	exportFlags   []string
	importFlags   []string
	failed        bool
}

func (m *Manager) convertRepo(info *resolver.RepositoryInfo, progressWriter io.Writer) error {
	plan, err := prepareConversion(info, progressWriter)
	if err != nil {
		return err
	}
	if !plan.isIncremental {
		defer func() {
			if plan.failed {
				os.RemoveAll(plan.targetPath)
			}
		}()
	}

	if err := runConversionPipeline(info.MirrorPath, plan.targetPath, plan.exportFlags, plan.importFlags, info.RepoName, progressWriter); err != nil {
		plan.failed = true
		return err
	}

	if err := m.finalizeConversion(info, plan.targetPath, plan.isIncremental); err != nil {
		plan.failed = true
		return err
	}

	return nil
}

func isIncrementalConversion(servingPath string) bool {
	for _, file := range []string{"HEAD", "evergit-sha1-marks.txt", "evergit-sha256-marks.txt"} {
		if _, err := os.Stat(filepath.Join(servingPath, file)); err != nil {
			return false
		}
	}
	return true
}

func prepareConversion(info *resolver.RepositoryInfo, progressWriter io.Writer) (*conversionPlan, error) {
	if err := os.MkdirAll(filepath.Dir(info.ServingPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create repos directory: %w", err)
	}

	isIncremental := isIncrementalConversion(info.ServingPath)
	var targetPath string

	if isIncremental {
		targetPath = info.ServingPath
		log.Printf("Starting high-performance incremental conversion for repository %s...", info.RepoName)
		if progressWriter != nil {
			_, _ = fmt.Fprintf(progressWriter, "remote: Evergit: Incremental update found. Performing incremental JIT conversion...\n")
		}
	} else {
		targetPath = filepath.Join(info.BuildsPath, strconv.FormatInt(time.Now().UnixNano(), 10))
		log.Printf("Starting full conversion for repository %s...", info.RepoName)
		if progressWriter != nil {
			_, _ = fmt.Fprintf(progressWriter, "remote: Evergit: Performing full JIT SHA-1 -> SHA-256 history conversion, please wait...\n")
		}
		if _, err := runCmd("", "git", "init", "--bare", "--object-format=sha256", targetPath); err != nil {
			return nil, fmt.Errorf("failed to initialize bare sha256 repository: %w", err)
		}
	}

	sha1Marks := filepath.Join(targetPath, "evergit-sha1-marks.txt")
	sha256Marks := filepath.Join(targetPath, "evergit-sha256-marks.txt")

	exportFlags := []string{
		"--all",
		"--signed-commits=strip",
		"--tag-of-filtered-object=rewrite",
		"--export-marks=" + sha1Marks,
	}
	importFlags := []string{
		"--export-marks=" + sha256Marks,
	}

	if isIncremental {
		exportFlags = append(exportFlags, "--import-marks="+sha1Marks)
		importFlags = append([]string{"--force", "--import-marks=" + sha256Marks}, importFlags...)
	}

	return &conversionPlan{
		targetPath:    targetPath,
		isIncremental: isIncremental,
		exportFlags:   exportFlags,
		importFlags:   importFlags,
	}, nil
}

func runConversionPipeline(mirrorPath, targetPath string, exportFlags, importFlags []string, repoName string, progressWriter io.Writer) error {
	exportCmd := exec.Command("git", append([]string{"fast-export"}, exportFlags...)...)
	exportCmd.Env = GitEnv()
	exportCmd.Dir = mirrorPath

	importCmd := exec.Command("git", append([]string{"fast-import"}, importFlags...)...)
	importCmd.Env = GitEnv()
	importCmd.Dir = targetPath

	r, w := io.Pipe()
	exportCmd.Stdout = w
	importCmd.Stdin = r

	var exportErrBuf, importErrBuf strings.Builder
	exportCmd.Stderr = &exportErrBuf

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

	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(importStderrPipe)
		for scanner.Scan() {
			line := scanner.Text()
			importErrBuf.WriteString(line + "\n")
			log.Printf("[%s fast-import] %s", repoName, line)
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

	<-stderrDone
	importErr := importCmd.Wait()
	_ = r.Close()

	exportErr := <-exportErrChan

	if importErr != nil {
		if m := gitlinkRejection.FindStringSubmatch(importErrBuf.String()); m != nil {
			return fmt.Errorf("%w: %s records SHA-1 commit %s at %q, which has no SHA-256 equivalent",
				ErrSubmodulesUnsupported, repoName, m[1], m[2])
		}
		return fmt.Errorf("git fast-import failed: %v (stderr: %q)", importErr, strings.TrimSpace(importErrBuf.String()))
	}
	if exportErr != nil {
		return fmt.Errorf("git fast-export failed: %v (stderr: %q)", exportErr, strings.TrimSpace(exportErrBuf.String()))
	}
	return nil
}

func (m *Manager) finalizeConversion(info *resolver.RepositoryInfo, targetPath string, isIncremental bool) error {
	if isIncremental {
		if err := pruneServingRefs(info); err != nil {
			return err
		}
	}

	headRef, err := runCmd(info.MirrorPath, "git", "symbolic-ref", "HEAD")
	if err != nil {
		headRef = "refs/heads/main"
	}

	if err := generateLooseObjectIdx(targetPath); err != nil {
		return fmt.Errorf("failed to generate loose-object-idx compatibility map: %w", err)
	}

	if _, err := runCmd(targetPath, "git", "--git-dir=.", "symbolic-ref", "HEAD", headRef); err != nil {
		log.Printf("WARNING: failed to set symbolic-ref HEAD in %s to %s: %v", targetPath, headRef, err)
	}

	now := time.Now()
	_ = os.Chtimes(filepath.Join(targetPath, "HEAD"), now, now)

	if !isIncremental {
		if err := publishBuild(info, targetPath); err != nil {
			return err
		}
	}

	m.logBackupRefs(info)
	return nil
}

// publishBuild atomically points the ServingPath symlink at buildPath, so readers always see
// either the old or the new repository. The previous build is kept for readers still using it;
// older builds are removed.
func publishBuild(info *resolver.RepositoryInfo, buildPath string) error {
	previous := ""
	if target, err := os.Readlink(info.ServingPath); err == nil {
		previous = filepath.Join(filepath.Dir(info.ServingPath), target)
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

	// Garbage collection of stale builds: only keep current buildPath and previous one
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

const (
	sha1HexLen   = 40
	sha256HexLen = 64
	// maxMarkID caps mark numbers to prevent corrupt marks files from creating runaway sparse scratch files.
	maxMarkID = 100_000_000
)

// generateLooseObjectIdx rebuilds objects/loose-object-idx by pairing the SHA1 and SHA256 marks files.
// It uses a fixed-offset scratch file on disk so the pairing runs in O(1) RAM without loading maps into memory.
// Pass 1 streams SHA1 marks into the scratch file at offset (markID-1)*sha1HexLen.
// Pass 2 streams the sequential SHA256 marks, matching each against the scratch file and emitting loose-object-idx.
func generateLooseObjectIdx(repoPath string) error {
	scratchPath := filepath.Join(repoPath, "objects", "loose-object-idx-scratch.tmp")
	scratchFile, err := os.Create(scratchPath)
	if err != nil {
		return fmt.Errorf("failed to create marks scratch file: %w", err)
	}
	defer func() {
		_ = scratchFile.Close()
		_ = os.Remove(scratchPath)
	}()

	sha1Path := filepath.Join(repoPath, "evergit-sha1-marks.txt")
	scratchSize, err := indexSHA1Marks(sha1Path, scratchFile)
	if err != nil {
		return err
	}

	sha256Path := filepath.Join(repoPath, "evergit-sha256-marks.txt")
	idxPath := filepath.Join(repoPath, "objects", "loose-object-idx")
	return writeLooseObjectIdx(sha256Path, scratchFile, scratchSize, idxPath)
}

// indexSHA1Marks streams unordered SHA1 marks into the scratch file at mark-indexed offsets (O(1) RAM).
func indexSHA1Marks(path string, scratch *os.File) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("failed to open SHA1 marks: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		mark, hash, ok := parseMarkLine(scanner.Text(), sha1HexLen)
		if !ok {
			continue
		}
		id, ok := parseMarkID(mark)
		if !ok {
			continue
		}
		offset := (id - 1) * sha1HexLen
		if _, err := scratch.WriteAt([]byte(hash), offset); err != nil {
			return 0, fmt.Errorf("failed to write mark %s to scratch file: %w", mark, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("failed to read SHA1 marks: %w", err)
	}

	info, err := scratch.Stat()
	if err != nil {
		return 0, fmt.Errorf("failed to stat scratch file: %w", err)
	}
	return info.Size(), nil
}

// writeLooseObjectIdx streams sequential SHA256 marks, looks up SHA1 hashes by offset, and atomically writes the index.
func writeLooseObjectIdx(sha256Path string, scratch *os.File, scratchSize int64, outPath string) error {
	sha256File, err := os.Open(sha256Path)
	if err != nil {
		return fmt.Errorf("failed to open SHA256 marks: %w", err)
	}
	defer sha256File.Close()

	tmpPath := outPath + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("failed to create loose-object-idx file: %w", err)
	}
	defer os.Remove(tmpPath)

	w := bufio.NewWriter(out)
	if _, err := w.WriteString("# loose-object-idx\n"); err != nil {
		_ = out.Close()
		return fmt.Errorf("failed to write header to loose-object-idx: %w", err)
	}

	sha1Reader := bufio.NewReaderSize(scratch, 256*1024)
	var currentOffset int64
	var sha1Buf [sha1HexLen]byte

	scanner := bufio.NewScanner(sha256File)
	for scanner.Scan() {
		mark, sha256Hash, ok := parseMarkLine(scanner.Text(), sha256HexLen)
		if !ok {
			continue
		}
		id, ok := parseMarkID(mark)
		if !ok {
			continue
		}
		targetOffset := (id - 1) * sha1HexLen
		if targetOffset+sha1HexLen > scratchSize {
			continue
		}

		// Handling Gaps or Out-of-Order Marks
		if targetOffset != currentOffset {
			if _, err := scratch.Seek(targetOffset, io.SeekStart); err != nil {
				_ = out.Close()
				return fmt.Errorf("failed to seek in scratch file: %w", err)
			}
			sha1Reader.Reset(scratch)
			currentOffset = targetOffset
		}

		if _, err := io.ReadFull(sha1Reader, sha1Buf[:]); err != nil {
			_ = out.Close()
			return fmt.Errorf("failed to read from scratch file: %w", err)
		}
		currentOffset += sha1HexLen

		if sha1Buf[0] == 0 {
			continue
		}

		_, _ = w.WriteString(sha256Hash)
		_ = w.WriteByte(' ')
		_, _ = w.Write(sha1Buf[:])
		_ = w.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		_ = out.Close()
		return fmt.Errorf("failed to read SHA256 marks: %w", err)
	}
	if err := w.Flush(); err != nil {
		_ = out.Close()
		return fmt.Errorf("failed to write loose-object-idx file: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("failed to close loose-object-idx file: %w", err)
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
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

// parseMarkID parses the integer ID from ":<id>".
func parseMarkID(mark string) (int64, bool) {
	if len(mark) < 2 || mark[0] != ':' {
		return 0, false
	}
	id, err := strconv.ParseInt(mark[1:], 10, 64)
	if err != nil || id <= 0 || id > maxMarkID {
		return 0, false
	}
	return id, true
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

// ErrSubmodulesUnsupported is returned for repositories whose history contains submodules: a
// submodule entry records a SHA-1 commit id of another repository, which cannot be translated
// without converting that repository too.
var ErrSubmodulesUnsupported = errors.New("repositories containing submodules cannot be converted to SHA-256 yet")

// gitlinkRejection matches fast-import refusing a submodule entry ("M 160000 <sha1> <path>").
var gitlinkRejection = regexp.MustCompile(`M 160000 ([0-9a-f]{40}) (.+)`)

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
		column, maxLen = 1, sha1HexLen
	case "sha256":
		column, maxLen = 0, sha256HexLen
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
