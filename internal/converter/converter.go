package converter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"evergit/internal/config"
	"evergit/internal/resolver"
)

// ErrServerBusy is returned when all conversion worker slots are occupied and the queue times out.
var ErrServerBusy = errors.New("server is busy performing other conversions, please retry shortly")

// ErrDiskSpaceExhausted is returned when free disk space is below the safety threshold.
var ErrDiskSpaceExhausted = errors.New("storage disk space exhausted")

// DefaultConversionTimeout is the maximum time a request will wait in the conversion queue before shedding load.
const DefaultConversionTimeout = 30 * time.Second

// DefaultExecTimeout is the maximum time a repository conversion may run before being terminated.
const DefaultExecTimeout = 1 * time.Hour

// DefaultMaxSubmoduleDepth is the maximum recursion depth for discovering and pre-converting active submodules.
const DefaultMaxSubmoduleDepth = 1

type Manager struct {
	cacheTTL          time.Duration
	minFreeDisk       uint64
	debug             bool
	conversionSem     chan struct{}
	conversionWait    time.Duration
	execTimeout       time.Duration
	maxSubmoduleDepth int
	cfg               *config.Config
	locksMu           sync.Mutex
	repoLocks         map[string]*repoLock
}

// repoJob tracks an active conversion job and its waiting clients.
type repoJob struct {
	mu       sync.Mutex
	waiters  int
	cancel   context.CancelFunc
	canceled bool
	done     chan struct{}
	err      error
}

// repoLock is a per-repository mutex, reference-counted so idle entries can be dropped.
type repoLock struct {
	mu    sync.Mutex
	users int
	job   *repoJob
}

func NewManager(cacheTTL time.Duration, maxConversions ...int) *Manager {
	limit := runtime.NumCPU()
	if len(maxConversions) > 0 && maxConversions[0] > 0 {
		limit = maxConversions[0]
	}
	if limit < 1 {
		limit = 1
	}
	return &Manager{
		cacheTTL:          cacheTTL,
		conversionSem:     make(chan struct{}, limit),
		conversionWait:    DefaultConversionTimeout,
		maxSubmoduleDepth: DefaultMaxSubmoduleDepth,
		repoLocks:         make(map[string]*repoLock),
	}
}

// SetMaxSubmoduleDepth sets the maximum recursion depth for active submodule conversions.
func (m *Manager) SetMaxSubmoduleDepth(depth int) {
	m.maxSubmoduleDepth = depth
}

// SetConfig sets the configuration used for resolving submodule paths and allowed hosts.
func (m *Manager) SetConfig(cfg *config.Config) {
	m.cfg = cfg
}

// SetDebug enables or disables debug mode (including fast-import statistics logging).
func (m *Manager) SetDebug(enabled bool) {
	m.debug = enabled
}

// SetMinFreeDisk configures the minimum free disk space in bytes required before cloning mirrors.
func (m *Manager) SetMinFreeDisk(bytes uint64) {
	m.minFreeDisk = bytes
}

// checkDiskSpace verifies that available disk space meets minFreeDisk threshold.
func (m *Manager) checkDiskSpace(path string) error {
	if m.minFreeDisk == 0 {
		return nil
	}
	var stat syscall.Statfs_t
	checkDir := path
	for {
		if fi, err := os.Stat(checkDir); err == nil && fi.IsDir() {
			break
		}
		parent := filepath.Dir(checkDir)
		if parent == checkDir {
			break
		}
		checkDir = parent
	}
	if err := syscall.Statfs(checkDir, &stat); err != nil {
		return nil
	}
	available := uint64(stat.Bavail) * uint64(stat.Bsize)
	if available < m.minFreeDisk {
		return fmt.Errorf("%w: available space (%.2f GB) is below minimum threshold (%.2f GB)",
			ErrDiskSpaceExhausted,
			float64(available)/(1024*1024*1024),
			float64(m.minFreeDisk)/(1024*1024*1024))
	}
	return nil
}

// SetConversionTimeout configures the maximum time an incoming conversion request waits in queue.
func (m *Manager) SetConversionTimeout(d time.Duration) {
	m.conversionWait = d
}

// SetExecTimeout configures the maximum duration a conversion pipeline may run.
func (m *Manager) SetExecTimeout(d time.Duration) {
	m.execTimeout = d
}

// TryAcquireConversionSlot acquires a conversion concurrency slot if available.
func (m *Manager) TryAcquireConversionSlot() bool {
	if m.conversionSem == nil {
		return false
	}
	select {
	case m.conversionSem <- struct{}{}:
		return true
	default:
		return false
	}
}

// ReleaseConversionSlot releases a conversion concurrency slot.
func (m *Manager) ReleaseConversionSlot() {
	if m.conversionSem == nil {
		return
	}
	select {
	case <-m.conversionSem:
	default:
	}
}

// WaitIdle blocks until all active background conversions are finished and locks are released.
func (m *Manager) WaitIdle() {
	for {
		m.locksMu.Lock()
		idle := len(m.repoLocks) == 0
		m.locksMu.Unlock()
		if idle {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type visitedReposKey struct{}

func withVisitedRepo(ctx context.Context, canonicalID string) (context.Context, bool) {
	visited, _ := ctx.Value(visitedReposKey{}).(map[string]bool)
	if visited == nil {
		visited = make(map[string]bool)
	}
	if visited[canonicalID] {
		return ctx, false
	}
	newVisited := make(map[string]bool, len(visited)+1)
	for k, v := range visited {
		newVisited[k] = v
	}
	newVisited[canonicalID] = true
	return context.WithValue(ctx, visitedReposKey{}, newVisited), true
}

// EnsureRepo checks if the repository exists, is up-to-date, and is converted.
// If not, it clones/fetches, converts it, and optionally streams progress.
// It accepts a context for cancellation (e.g. when all waiting clients disconnect).
func (m *Manager) EnsureRepo(ctx context.Context, info *resolver.RepositoryInfo, progressWriter io.Writer) error {
	depth := m.maxSubmoduleDepth
	if depth < 0 {
		depth = DefaultMaxSubmoduleDepth
	}
	return m.EnsureRepoWithDepth(ctx, info, progressWriter, depth)
}

// EnsureRepoWithDepth checks or converts a repository with bounded recursion depth for active submodules.
func (m *Manager) EnsureRepoWithDepth(ctx context.Context, info *resolver.RepositoryInfo, progressWriter io.Writer, depth int) error {
	if ctx == nil {
		ctx = context.Background()
	}
	canonicalID := info.Domain + "/" + info.Owner + "/" + info.RepoName
	ctxWithVisited, ok := withVisitedRepo(ctx, canonicalID)
	if !ok {
		// Cycle detected: this repository is already in the parent conversion chain
		return nil
	}
	ctx = ctxWithVisited

	if progressWriter != nil {
		async := newAsyncWriter(progressWriter)
		defer async.Close()
		progressWriter = async
	}

	m.locksMu.Lock()
	lock, exists := m.repoLocks[info.ServingPath]
	if !exists {
		lock = &repoLock{}
		m.repoLocks[info.ServingPath] = lock
	}
	lock.users++
	m.locksMu.Unlock()

	defer func() {
		m.locksMu.Lock()
		lock.users--
		if lock.users == 0 && lock.job == nil {
			delete(m.repoLocks, info.ServingPath)
		}
		m.locksMu.Unlock()
	}()

	lock.mu.Lock()
	if lock.job != nil {
		job := lock.job
		if !job.canceled {
			lock.mu.Unlock()
			return m.waitJob(ctx, lock, job)
		}
		// Previous job was canceled and is shutting down; wait for it to exit before starting anew
		lock.mu.Unlock()
		select {
		case <-job.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		lock.mu.Lock()
	}

	// 1. Check if the repository already exists and is within its TTL
	if !m.needsSync(info) {
		lock.mu.Unlock()
		return nil
	}

	job := &repoJob{done: make(chan struct{})}
	timeout := m.execTimeout
	if timeout <= 0 {
		timeout = DefaultExecTimeout
	}
	jobCtx, cancel := context.WithTimeout(context.Background(), timeout)
	job.cancel = cancel
	lock.job = job
	lock.mu.Unlock()

	go func() {
		err := m.doEnsureRepo(jobCtx, info, progressWriter, depth)
		job.mu.Lock()
		job.err = err
		close(job.done)
		job.mu.Unlock()

		m.locksMu.Lock()
		lock.mu.Lock()
		if lock.job == job {
			lock.job = nil
		}
		if lock.users == 0 {
			delete(m.repoLocks, info.ServingPath)
		}
		lock.mu.Unlock()
		m.locksMu.Unlock()
	}()

	return m.waitJob(ctx, lock, job)
}

func (m *Manager) waitJob(ctx context.Context, lock *repoLock, job *repoJob) error {
	job.mu.Lock()
	job.waiters++
	job.mu.Unlock()

	defer func() {
		job.mu.Lock()
		job.waiters--
		if job.waiters == 0 {
			select {
			case <-job.done:
			default:
				job.canceled = true
				job.cancel()
			}
		}
		job.mu.Unlock()
	}()

	select {
	case <-job.done:
		job.mu.Lock()
		err := job.err
		job.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) doEnsureRepo(ctx context.Context, info *resolver.RepositoryInfo, progressWriter io.Writer, depth int) error {
	// 1. Check if the repository already exists and is within its TTL
	if !m.needsSync(info) {
		return nil
	}

	// 2. Clone/Fetch the upstream mirror
	changed, err := m.syncMirror(ctx, info)
	if err != nil {
		return err
	}

	// If the upstream mirror is natively SHA-256, bypass the fast-export | fast-import pipeline
	if isRepoSHA256(info.MirrorPath) {
		return m.syncNativeSHA256(ctx, info, changed, progressWriter)
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

	// 3. Opportunistically pre-convert active submodules and load their commit translation maps
	submoduleCommitMap := m.prepareSubmodules(ctx, info, progressWriter, depth)

	// 4. Convert mirror to serving repo (SHA1 -> SHA256 conversion)
	if err := m.convertRepo(ctx, info, progressWriter, submoduleCommitMap); err != nil {
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
func (m *Manager) syncNativeSHA256(ctx context.Context, info *resolver.RepositoryInfo, changed bool, progressWriter io.Writer) error {
	servingHeadPath := filepath.Join(info.ServingPath, "HEAD")
	if _, err := os.Stat(servingHeadPath); err == nil {
		if !changed {
			now := time.Now()
			_ = os.Chtimes(servingHeadPath, now, now)
			return nil
		}

		log.Printf("Syncing native SHA256 updates for repository %s...", info.RepoName)
		if progressWriter != nil {
			_, _ = fmt.Fprintf(progressWriter, "remote: Evergit: Syncing native SHA-256 updates for %s...\n", info.RepoName)
		}
		// Fetch branches and tags, pruning any deleted upstream branches/tags
		if _, err := runCmdContext(ctx, 0, info.ServingPath, "git", "fetch", "--prune", "--", info.MirrorPath,
			"+refs/heads/*:refs/heads/*",
			"+refs/tags/*:refs/tags/*",
		); err != nil {
			return fmt.Errorf("failed to fetch native sha256 updates into %s: %w", info.ServingPath, err)
		}

		// Backup refs are never pruned: older versions or re-cloned mirrors may lose them,
		// leaving the serving repo as their only copy. Fetch them additively without --prune.
		if _, err := runCmdContext(ctx, 0, info.ServingPath, "git", "fetch", "--", info.MirrorPath,
			"+refs/evergit-backups/*:refs/evergit-backups/*",
		); err != nil {
			log.Printf("WARNING: failed to fetch backup refs for native sha256 repo %s: %v", info.RepoName, err)
		}

		if headRef, err := runCmdContext(ctx, 0, info.MirrorPath, "git", "symbolic-ref", "HEAD"); err == nil {
			_, _ = runCmdContext(ctx, 0, info.ServingPath, "git", "--git-dir=.", "symbolic-ref", "HEAD", headRef)
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
		_, _ = fmt.Fprintf(progressWriter, "remote: Evergit: Upstream %s is native SHA-256. Bypassing conversion pipeline...\n", info.RepoName)
	}
	if _, err := runCmdContext(ctx, 0, "", "git", "clone", "--bare", "--", info.MirrorPath, buildPath); err != nil {
		os.RemoveAll(buildPath)
		return fmt.Errorf("failed to clone native sha256 repository %s: %w", info.RepoName, err)
	}

	// clone --bare only copies heads and tags; also copy any existing backup refs
	_, _ = runCmdContext(ctx, 0, buildPath, "git", "fetch", "--", info.MirrorPath, "+refs/evergit-backups/*:refs/evergit-backups/*")

	if headRef, err := runCmdContext(ctx, 0, info.MirrorPath, "git", "symbolic-ref", "HEAD"); err == nil {
		_, _ = runCmdContext(ctx, 0, buildPath, "git", "--git-dir=.", "symbolic-ref", "HEAD", headRef)
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

func (m *Manager) syncMirror(ctx context.Context, info *resolver.RepositoryInfo) (bool, error) {
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
		if err := m.checkDiskSpace(info.MirrorPath); err != nil {
			return false, err
		}
		// Clone as a bare mirror
		// --bare rather than --mirror: only branches and tags, not forge refs such as GitHub's
		// thousands of refs/pull/* from forks
		_, err := runCmdContext(ctx, upstreamTimeout, "", "git", "-c", "http.followRedirects=false", "clone", "--bare", "--", info.RemoteURL, info.MirrorPath)
		if err != nil {
			_ = os.RemoveAll(info.MirrorPath)
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
		fetchArgs := append([]string{"-c", "http.followRedirects=false", "fetch", "--prune", "origin"}, mirrorRefspecs...)
		_, err = runCmdContext(ctx, upstreamTimeout, info.MirrorPath, "git", fetchArgs...)

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
	totalObjects  int
	failed        bool
}

type submoduleEntry struct {
	Name string
	Path string
	URL  string
}

func parseGitmodulesConfig(out string) []submoduleEntry {
	subMap := make(map[string]*submoduleEntry)
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		key, val, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		parts := strings.Split(key, ".")
		if len(parts) >= 3 && parts[0] == "submodule" {
			name := strings.Join(parts[1:len(parts)-1], ".")
			prop := parts[len(parts)-1]
			entry := subMap[name]
			if entry == nil {
				entry = &submoduleEntry{Name: name}
				subMap[name] = entry
			}
			switch prop {
			case "path":
				entry.Path = val
			case "url":
				entry.URL = val
			}
		}
	}
	var res []submoduleEntry
	for _, entry := range subMap {
		if entry.Path != "" && entry.URL != "" {
			res = append(res, *entry)
		}
	}
	return res
}

func listActiveSubmodules(ctx context.Context, mirrorPath string) ([]submoduleEntry, error) {
	uniqueEntries := make(map[string]submoduleEntry)

	// 1. Check HEAD:.gitmodules
	if out, err := runCmdContext(ctx, 10*time.Second, mirrorPath, "git", "config", "--blob", "HEAD:.gitmodules", "--list"); err == nil {
		for _, entry := range parseGitmodulesConfig(out) {
			key := entry.Path + "\x00" + entry.URL
			uniqueEntries[key] = entry
		}
	}

	// 2. Discover historical submodules and previous paths across git history
	if commitsOut, err := runCmdContext(ctx, 10*time.Second, mirrorPath, "git", "log", "--all", "--format=%H", "-n", "50", "--", ".gitmodules"); err == nil {
		for _, commit := range strings.Fields(commitsOut) {
			if blobOut, err := runCmdContext(ctx, 5*time.Second, mirrorPath, "git", "config", "--blob", commit+":.gitmodules", "--list"); err == nil {
				for _, entry := range parseGitmodulesConfig(blobOut) {
					key := entry.Path + "\x00" + entry.URL
					if _, exists := uniqueEntries[key]; !exists {
						uniqueEntries[key] = entry
					}
				}
			}
		}
	}

	if len(uniqueEntries) == 0 {
		return nil, nil
	}

	res := make([]submoduleEntry, 0, len(uniqueEntries))
	for _, entry := range uniqueEntries {
		res = append(res, entry)
	}
	return res, nil
}

func (m *Manager) resolveSubmoduleInfo(rawURL string, parentInfo *resolver.RepositoryInfo) (*resolver.RepositoryInfo, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, errors.New("empty submodule URL")
	}

	storageRoot := ""
	if m.cfg != nil && m.cfg.StorageRoot != "" {
		storageRoot = m.cfg.StorageRoot
	} else if parentInfo.MirrorPath != "" {
		storageRoot = filepath.Dir(filepath.Dir(filepath.Dir(parentInfo.MirrorPath)))
	}

	// Handle local/fixture repos used in testing
	if parentInfo.Domain == "local" || strings.HasPrefix(rawURL, "/") || strings.HasPrefix(rawURL, "file://") {
		localPath := strings.TrimPrefix(rawURL, "file://")
		if !filepath.IsAbs(localPath) && parentInfo.RemoteURL != "" {
			parentDir := filepath.Dir(parentInfo.RemoteURL)
			localPath = filepath.Clean(filepath.Join(parentDir, localPath))
		}
		repoName := strings.TrimSuffix(filepath.Base(localPath), ".git")
		if storageRoot == "" {
			storageRoot = "./storage"
		}
		return &resolver.RepositoryInfo{
			Domain:      "local",
			Owner:       "submodule",
			RepoName:    repoName,
			RemoteURL:   localPath,
			MirrorPath:  filepath.Join(storageRoot, "mirrors", "local", "submodule", repoName+".git"),
			ServingPath: filepath.Join(storageRoot, "repos", "local", "submodule", repoName+".git"),
			BuildsPath:  filepath.Join(storageRoot, "builds", "local", "submodule", repoName+".git"),
		}, nil
	}

	cfg := m.cfg
	if cfg == nil {
		cfg = &config.Config{
			StorageRoot:  storageRoot,
			AllowedHosts: config.DefaultAllowedHosts,
			Shorthands:   map[string]string{"github": "github.com", "gitlab": "gitlab.com", "bitbucket": "bitbucket.org"},
		}
	}

	var targetPath string
	if strings.HasPrefix(rawURL, "./") || strings.HasPrefix(rawURL, "../") {
		// Relative URL: relative to parent repository's remote path
		parentBase := path.Join(parentInfo.Domain, parentInfo.Owner)
		targetPath = path.Clean(path.Join(parentBase, rawURL))
	} else if strings.HasPrefix(rawURL, "git@") {
		// git@domain:owner/repo.git
		trimmed := strings.TrimPrefix(rawURL, "git@")
		domain, repoPart, found := strings.Cut(trimmed, ":")
		if !found {
			return nil, fmt.Errorf("invalid git@ submodule URL %q", rawURL)
		}
		targetPath = domain + "/" + repoPart
	} else if strings.Contains(rawURL, "://") {
		u, err := url.Parse(rawURL)
		if err != nil {
			return nil, fmt.Errorf("failed to parse submodule URL %q: %w", rawURL, err)
		}
		host := u.Hostname()
		repoPart := strings.TrimPrefix(u.Path, "/")
		targetPath = host + "/" + repoPart
	} else {
		targetPath = rawURL
	}

	targetPath = strings.TrimSuffix(targetPath, ".git")
	return resolver.ParsePath(targetPath, cfg)
}

func loadSubmoduleMarks(servingPath string) (map[string]string, error) {
	sha1Path := filepath.Join(servingPath, "evergit-sha1-marks.txt")
	sha256Path := filepath.Join(servingPath, "evergit-sha256-marks.txt")

	f1, err := os.Open(sha1Path)
	if err != nil {
		return nil, err
	}
	defer f1.Close()

	markToSHA1 := make(map[string]string)
	s1 := bufio.NewScanner(f1)
	for s1.Scan() {
		line := s1.Text()
		mark, sha, ok := parseMarkLine(line, sha1HexLen)
		if ok {
			markToSHA1[mark] = sha
		}
	}
	if err := s1.Err(); err != nil {
		return nil, err
	}

	f2, err := os.Open(sha256Path)
	if err != nil {
		return nil, err
	}
	defer f2.Close()

	sha1ToSHA256 := make(map[string]string, len(markToSHA1))
	s2 := bufio.NewScanner(f2)
	for s2.Scan() {
		line := s2.Text()
		mark, sha256, ok := parseMarkLine(line, sha256HexLen)
		if ok {
			if sha1, found := markToSHA1[mark]; found {
				sha1ToSHA256[sha1] = sha256
			}
		}
	}
	if err := s2.Err(); err != nil {
		return nil, err
	}

	return sha1ToSHA256, nil
}

func (m *Manager) prepareSubmodules(ctx context.Context, info *resolver.RepositoryInfo, progressWriter io.Writer, depth int) map[string]map[string]string {
	submodules, err := listActiveSubmodules(ctx, info.MirrorPath)
	if err != nil || len(submodules) == 0 {
		return nil
	}

	submoduleCommitMap := make(map[string]map[string]string)
	marksCache := make(map[string]map[string]string)

	for _, entry := range submodules {
		subInfo, err := m.resolveSubmoduleInfo(entry.URL, info)
		if err != nil {
			if m.debug {
				log.Printf("[%s] Submodule %q (%s) cannot be resolved: %v", info.RepoName, entry.Path, entry.URL, err)
			}
			continue
		}

		if depth > 0 {
			if err := m.EnsureRepoWithDepth(ctx, subInfo, progressWriter, depth-1); err != nil {
				log.Printf("[%s] Warning: failed to pre-convert submodule %q: %v", info.RepoName, entry.Path, err)
			}
		}

		marks, cached := marksCache[subInfo.ServingPath]
		if !cached {
			if loaded, err := loadSubmoduleMarks(subInfo.ServingPath); err == nil && len(loaded) > 0 {
				marks = loaded
				marksCache[subInfo.ServingPath] = marks
				if m.debug {
					log.Printf("[%s] Loaded %d commit translations for submodule repo %s", info.RepoName, len(marks), subInfo.ServingPath)
				}
			} else {
				marksCache[subInfo.ServingPath] = nil
			}
		}

		if len(marks) > 0 {
			submoduleCommitMap[entry.Path] = marks
			if m.debug {
				log.Printf("[%s] Mapped %d commit translations to submodule path %q", info.RepoName, len(marks), entry.Path)
			}
		}
	}
	return submoduleCommitMap
}

func (m *Manager) convertRepo(ctx context.Context, info *resolver.RepositoryInfo, progressWriter io.Writer, submoduleCommitMap map[string]map[string]string) error {
	// Guard against unbounded concurrent conversions across all repositories
	if m.conversionSem != nil {
		wait := m.conversionWait
		if wait <= 0 {
			wait = DefaultConversionTimeout
		}
		select {
		case m.conversionSem <- struct{}{}:
			defer func() { <-m.conversionSem }()
		case <-time.After(wait):
			return ErrServerBusy
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	plan, err := m.prepareConversion(ctx, info, progressWriter)
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

	if err := runConversionPipeline(ctx, info.MirrorPath, plan.targetPath, plan.exportFlags, plan.importFlags, info.RepoName, plan.totalObjects, plan.isIncremental, m.debug, progressWriter, submoduleCommitMap); err != nil {
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

func countExportableObjects(ctx context.Context, mirrorPath string) int {
	commitsOut, err := runCmdContext(ctx, 0, mirrorPath, "git", "rev-list", "--all", "--count")
	if err != nil {
		return 0
	}
	commits, _ := strconv.Atoi(strings.TrimSpace(commitsOut))

	blobsOut, err := runCmdContext(ctx, 0, mirrorPath, "git", "rev-list", "--objects", "--filter=object:type=blob", "--all", "--count")
	if err != nil {
		return commits
	}
	blobs, _ := strconv.Atoi(strings.TrimSpace(blobsOut))

	return commits + blobs
}

func (m *Manager) prepareConversion(ctx context.Context, info *resolver.RepositoryInfo, progressWriter io.Writer) (*conversionPlan, error) {
	if err := os.MkdirAll(filepath.Dir(info.ServingPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create repos directory: %w", err)
	}

	isIncremental := isIncrementalConversion(info.ServingPath)
	var targetPath string
	totalObjects := 0

	if isIncremental {
		targetPath = info.ServingPath
		log.Printf("Starting high-performance incremental conversion for repository %s...", info.RepoName)
		if progressWriter != nil {
			_, _ = fmt.Fprintf(progressWriter, "remote: Evergit: [%s] Incremental update found. Performing incremental JIT conversion...\n", info.RepoName)
		}
	} else {
		targetPath = filepath.Join(info.BuildsPath, strconv.FormatInt(time.Now().UnixNano(), 10))
		log.Printf("Starting full conversion for repository %s...", info.RepoName)
		if progressWriter != nil {
			_, _ = fmt.Fprintf(progressWriter, "remote: Evergit: [%s] Performing full JIT SHA-1 -> SHA-256 history conversion, please wait...\n", info.RepoName)
		}
		if _, err := runCmdContext(ctx, 0, "", "git", "init", "--bare", "--object-format=sha256", targetPath); err != nil {
			return nil, fmt.Errorf("failed to initialize bare sha256 repository: %w", err)
		}
		totalObjects = countExportableObjects(ctx, info.MirrorPath)
	}

	sha1Marks := filepath.Join(targetPath, "evergit-sha1-marks.txt")
	sha256Marks := filepath.Join(targetPath, "evergit-sha256-marks.txt")

	interval := progressObjectInterval
	if totalObjects > 0 {
		// Target ~50 progress updates across the conversion, but no more frequently than every 5,000 objects
		calculated := totalObjects / 50
		if calculated > interval {
			interval = calculated
		}
	}

	exportFlags := []string{
		"--all",
		"--signed-commits=strip",
		"--signed-tags=strip",
		"--tag-of-filtered-object=rewrite",
		"--mark-tags",
		fmt.Sprintf("--progress=%d", interval),
		"--export-marks=" + sha1Marks,
	}
	importFlags := []string{
		"--export-marks=" + sha256Marks,
	}
	if m.debug {
		importFlags = append(importFlags, "--stats")
	} else {
		importFlags = append(importFlags, "--quiet")
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
		totalObjects:  totalObjects,
	}, nil
}

const (
	syntheticSHA256Padding = "000000000000000000000000" // 24 zeros
	nullSHA1               = "0000000000000000000000000000000000000000"
	nullSHA256             = "0000000000000000000000000000000000000000000000000000000000000000"
)

// commitFileCmd represents a file modification command within a commit block.
type commitFileCmd struct {
	line       string
	data       []byte
	isDelete   bool
	targetPath string
}

func extractPath(raw string) string {
	raw = strings.TrimRight(raw, "\r\n")
	if strings.HasPrefix(raw, "\"") {
		if unquoted, err := strconv.Unquote(raw); err == nil {
			return unquoted
		}
	}
	return raw
}

func extractTargetFromCopyRename(line string) string {
	trimmed := strings.TrimRight(line, "\r\n")
	if len(trimmed) < 2 {
		return ""
	}
	rest := strings.TrimSpace(trimmed[2:])
	if strings.HasPrefix(rest, "\"") {
		endQuote := -1
		escaped := false
		for i := 1; i < len(rest); i++ {
			if escaped {
				escaped = false
				continue
			}
			if rest[i] == '\\' {
				escaped = true
				continue
			}
			if rest[i] == '"' {
				endQuote = i
				break
			}
		}
		if endQuote != -1 && endQuote+1 < len(rest) {
			dst := strings.TrimSpace(rest[endQuote+1:])
			return extractPath(dst)
		}
	}
	parts := strings.SplitN(rest, " ", 2)
	if len(parts) == 2 {
		return extractPath(parts[1])
	}
	return ""
}

func dirPrefixes(path string) []string {
	var prefixes []string
	last := 0
	for {
		idx := strings.IndexByte(path[last:], '/')
		if idx == -1 {
			break
		}
		prefix := path[:last+idx]
		if len(prefix) > 0 {
			prefixes = append(prefixes, prefix)
		}
		last += idx + 1
	}
	return prefixes
}

// reorderCommitFileCommands fixes a Git fast-export bug where git fast-export orders file additions
// under a new directory BEFORE the D command that deletes an old file or symlink at that directory path.
// When git fast-import executes D <path>, it treats it as a recursive tree deletion and wipes out the
// directory and its files that were just created.
// To prevent this, any D command whose path is an ancestor directory of any file modified or added in
// the commit is moved before the first command affecting that directory subtree.
func reorderCommitFileCommands(cmds []commitFileCmd) []commitFileCmd {
	if len(cmds) <= 1 {
		return cmds
	}

	delIndices := make(map[string]int)
	hasDeletes := false
	for i, cmd := range cmds {
		if cmd.isDelete && cmd.targetPath != "" {
			delIndices[cmd.targetPath] = i
			hasDeletes = true
		}
	}
	if !hasDeletes {
		return cmds
	}

	// minTarget maps delIdx -> earliest child idx that appeared before delIdx
	minTarget := make(map[int]int)
	for i, cmd := range cmds {
		if cmd.targetPath == "" {
			continue
		}
		for _, prefix := range dirPrefixes(cmd.targetPath) {
			if delIdx, ok := delIndices[prefix]; ok && delIdx > i {
				if earliest, seen := minTarget[delIdx]; !seen || i < earliest {
					minTarget[delIdx] = i
				}
			}
		}
	}

	if len(minTarget) == 0 {
		return cmds
	}

	type moveItem struct {
		delIdx   int
		insertAt int
		path     string
	}
	var toMove []moveItem
	movedIndices := make(map[int]bool)
	for delIdx, insertAt := range minTarget {
		toMove = append(toMove, moveItem{
			delIdx:   delIdx,
			insertAt: insertAt,
			path:     cmds[delIdx].targetPath,
		})
		movedIndices[delIdx] = true
	}

	// Sort toMove: primary by insertAt ascending, secondary by path length ascending (shorter parent paths first)
	sort.Slice(toMove, func(i, j int) bool {
		if toMove[i].insertAt != toMove[j].insertAt {
			return toMove[i].insertAt < toMove[j].insertAt
		}
		return len(toMove[i].path) < len(toMove[j].path)
	})

	insertions := make(map[int][]commitFileCmd)
	for _, item := range toMove {
		insertions[item.insertAt] = append(insertions[item.insertAt], cmds[item.delIdx])
	}

	result := make([]commitFileCmd, 0, len(cmds))
	for i := 0; i < len(cmds); i++ {
		if toInsert, ok := insertions[i]; ok {
			result = append(result, toInsert...)
		}
		if !movedIndices[i] {
			result = append(result, cmds[i])
		}
	}

	return result
}

func flushCommitFileCommands(w io.Writer, cmds []commitFileCmd) error {
	if len(cmds) == 0 {
		return nil
	}
	reordered := reorderCommitFileCommands(cmds)
	for _, cmd := range reordered {
		if _, err := io.WriteString(w, cmd.line); err != nil {
			return err
		}
		if len(cmd.data) > 0 {
			if _, err := w.Write(cmd.data); err != nil {
				return err
			}
		}
	}
	return nil
}

// transformFastExportStream filters the fast-export stream, zero-padding 40-character SHA-1
// gitlinks (mode 160000) to 64-character synthetic SHA-256 hashes (or replacing them with the real
// SHA-256 commit hash if available in submoduleCommitMap) so that fast-import accepts them into a
// SHA-256 repository without rejecting submodules. It also translates 40-character null SHA-1 references
// (emitted e.g. when dereferencing nested tags with --mark-tags) to 64-character null SHA-256 references.
// It also reorders file commands within commits so deletions of files/symlinks replaced by directories
// precede additions in those directories, preventing fast-import from deleting the newly created directories.
// Binary data payloads are copied verbatim.
func transformFastExportStream(r io.Reader, w io.Writer, submoduleCommitMap map[string]map[string]string) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var commitCmds []commitFileCmd
	inCommit := false

	flushCommit := func() error {
		if inCommit {
			if err := flushCommitFileCommands(w, commitCmds); err != nil {
				return err
			}
			commitCmds = nil
			inCommit = false
		}
		return nil
	}

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			// Check if this line marks the start of a new top-level command or boundary
			if strings.HasPrefix(line, "commit ") {
				if err := flushCommit(); err != nil {
					return err
				}
				inCommit = true
				if _, werr := io.WriteString(w, line); werr != nil {
					return werr
				}
				continue
			} else if inCommit && (line == "\n" || line == "\r\n" ||
				strings.HasPrefix(line, "tag ") ||
				strings.HasPrefix(line, "reset ") ||
				strings.HasPrefix(line, "blob") ||
				strings.HasPrefix(line, "checkpoint") ||
				strings.HasPrefix(line, "progress ") ||
				strings.HasPrefix(line, "feature ")) {
				if err := flushCommit(); err != nil {
					return err
				}
			}

			// Handle data payloads
			if strings.HasPrefix(line, "data ") {
				sizeStr := strings.TrimSpace(line[5:])
				size, parseErr := strconv.ParseInt(sizeStr, 10, 64)
				if parseErr == nil && size >= 0 {
					if inCommit && len(commitCmds) > 0 {
						// This data payload belongs to the preceding inline file command (e.g. M ... inline)
						dataBuf := make([]byte, size)
						if _, rerr := io.ReadFull(br, dataBuf); rerr != nil {
							return rerr
						}
						lastIdx := len(commitCmds) - 1
						commitCmds[lastIdx].line += line
						commitCmds[lastIdx].data = dataBuf
						continue
					}
					// Otherwise, it is a commit message, blob, or tag payload: stream directly
					if _, werr := io.WriteString(w, line); werr != nil {
						return werr
					}
					if size > 0 {
						if _, cerr := io.CopyN(w, br, size); cerr != nil {
							return cerr
						}
					}
					continue
				}
			}

			if inCommit {
				if strings.HasPrefix(line, "M 160000 ") {
					// M 160000 <dataref> <path>
					parts := strings.SplitN(line, " ", 4)
					if len(parts) == 4 && len(parts[2]) == 40 {
						sha1 := parts[2]
						subPath := extractPath(parts[3])
						var targetSHA string
						if submoduleCommitMap != nil {
							if m, ok := submoduleCommitMap[subPath]; ok {
								if realSHA, ok := m[sha1]; ok && len(realSHA) == 64 {
									targetSHA = realSHA
								}
							}
							if targetSHA == "" {
								// If not found under the exact subPath (e.g. submodule moved or path varied in history),
								// search across all other pre-converted submodules. Commit SHA-1s are unique.
								for _, m := range submoduleCommitMap {
									if realSHA, ok := m[sha1]; ok && len(realSHA) == 64 {
										targetSHA = realSHA
										break
									}
								}
							}
						}
						if targetSHA == "" {
							targetSHA = sha1 + syntheticSHA256Padding
						}
						line = fmt.Sprintf("M 160000 %s %s", targetSHA, parts[3])
					}
					commitCmds = append(commitCmds, commitFileCmd{
						line:       line,
						isDelete:   false,
						targetPath: extractPath(strings.SplitN(line, " ", 4)[3]),
					})
					continue
				} else if strings.HasPrefix(line, "M ") {
					parts := strings.SplitN(line, " ", 4)
					var p string
					if len(parts) >= 4 {
						p = extractPath(parts[3])
					}
					commitCmds = append(commitCmds, commitFileCmd{
						line:       line,
						isDelete:   false,
						targetPath: p,
					})
					continue
				} else if strings.HasPrefix(line, "D ") {
					p := extractPath(strings.TrimPrefix(line, "D "))
					commitCmds = append(commitCmds, commitFileCmd{
						line:       line,
						isDelete:   true,
						targetPath: p,
					})
					continue
				} else if strings.HasPrefix(line, "C ") || strings.HasPrefix(line, "R ") {
					p := extractTargetFromCopyRename(line)
					commitCmds = append(commitCmds, commitFileCmd{
						line:       line,
						isDelete:   false,
						targetPath: p,
					})
					continue
				} else if strings.HasPrefix(line, "deleteall") || strings.HasPrefix(line, "N ") {
					commitCmds = append(commitCmds, commitFileCmd{
						line: line,
					})
					continue
				} else if strings.HasPrefix(line, "from ") || strings.HasPrefix(line, "merge ") {
					parts := strings.SplitN(line, " ", 2)
					if len(parts) == 2 && strings.TrimRight(parts[1], "\r\n ") == nullSHA1 {
						line = parts[0] + " " + nullSHA256 + "\n"
					}
				}
			} else {
				if strings.HasPrefix(line, "from ") || strings.HasPrefix(line, "merge ") {
					parts := strings.SplitN(line, " ", 2)
					if len(parts) == 2 && strings.TrimRight(parts[1], "\r\n ") == nullSHA1 {
						line = parts[0] + " " + nullSHA256 + "\n"
					}
				}
			}

			if _, werr := io.WriteString(w, line); werr != nil {
				return werr
			}
		}
		if err != nil {
			if err == io.EOF {
				return flushCommit()
			}
			return err
		}
	}
}

func runConversionPipeline(ctx context.Context, mirrorPath, targetPath string, exportFlags, importFlags []string, repoName string, totalObjects int, isIncremental, debug bool, progressWriter io.Writer, submoduleCommitMap map[string]map[string]string) error {
	exportCmd := exec.CommandContext(ctx, "git", append([]string{"fast-export"}, exportFlags...)...)
	exportCmd.Env = GitEnv()
	exportCmd.Dir = mirrorPath

	importCmd := exec.CommandContext(ctx, "git", append([]string{"fast-import"}, importFlags...)...)
	importCmd.Env = GitEnv()
	importCmd.Dir = targetPath

	exportReader, exportWriter := io.Pipe()
	importReader, importWriter := io.Pipe()

	exportCmd.Stdout = exportWriter
	importCmd.Stdin = importReader

	var exportErrBuf, importErrBuf strings.Builder
	exportCmd.Stderr = &exportErrBuf

	importStdoutPipe, err := importCmd.StdoutPipe()
	if err != nil {
		_ = exportReader.Close()
		_ = exportWriter.Close()
		_ = importReader.Close()
		_ = importWriter.Close()
		return fmt.Errorf("failed to create stdout pipe for git fast-import: %w", err)
	}

	importStderrPipe, err := importCmd.StderrPipe()
	if err != nil {
		_ = exportReader.Close()
		_ = exportWriter.Close()
		_ = importReader.Close()
		_ = importWriter.Close()
		_ = importStdoutPipe.Close()
		return fmt.Errorf("failed to create stderr pipe for git fast-import: %w", err)
	}

	if err := exportCmd.Start(); err != nil {
		_ = exportReader.Close()
		_ = exportWriter.Close()
		_ = importReader.Close()
		_ = importWriter.Close()
		return fmt.Errorf("failed to start git fast-export: %w", err)
	}
	if err := importCmd.Start(); err != nil {
		_ = exportReader.Close()
		_ = exportWriter.Close()
		_ = importReader.Close()
		_ = importWriter.Close()
		_ = exportCmd.Process.Kill()
		_ = exportCmd.Wait()
		return fmt.Errorf("failed to start git fast-import: %w", err)
	}

	transformErrChan := make(chan error, 1)
	go func() {
		err := transformFastExportStream(exportReader, importWriter, submoduleCommitMap)
		_ = exportReader.CloseWithError(err)
		_ = importWriter.CloseWithError(err)
		transformErrChan <- err
	}()

	var pipesWg sync.WaitGroup
	pipesWg.Add(2)

	// Stream stdout of fast-import in real-time (contains "--progress" statements from fast-export)
	go func() {
		defer pipesWg.Done()
		scanner := bufio.NewScanner(importStdoutPipe)
		for scanner.Scan() {
			line := scanner.Text()
			if debug {
				log.Printf("[%s fast-import] %s", repoName, line)
			}
			if progressWriter != nil {
				if totalObjects > 0 {
					fields := strings.Fields(line)
					if len(fields) >= 2 {
						if current, err := strconv.Atoi(fields[1]); err == nil {
							pct := int((float64(current) / float64(totalObjects)) * 100)
							if pct > 99 {
								pct = 99
							}
							_, _ = fmt.Fprintf(progressWriter, "\rremote: Evergit: [Progress] Converting %s: %2d%%...", repoName, pct)
						}
					}
				} else if isIncremental {
					_, _ = fmt.Fprintf(progressWriter, "\rremote: Evergit: [Progress] Converting %s: incremental updates...\r", repoName)
				}
			}
		}
	}()

	// Stream stderr of fast-import in real-time, keeping it for error reporting and stats logging
	go func() {
		defer pipesWg.Done()
		reader := bufio.NewReader(importStderrPipe)
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				importErrBuf.WriteString(line)
				if debug {
					log.Printf("[%s fast-import] %s", repoName, strings.TrimRight(line, "\r\n"))
				}
			}
			if err != nil {
				break
			}
		}
	}()

	exportErrChan := make(chan error, 1)
	go func() {
		err := exportCmd.Wait()
		_ = exportWriter.CloseWithError(err)
		exportErrChan <- err
	}()

	// All stdout and stderr reads must complete before Wait closes the pipes
	pipesWg.Wait()
	importErr := importCmd.Wait()
	_ = importReader.Close()

	if progressWriter != nil {
		if totalObjects > 0 {
			_, _ = fmt.Fprintf(progressWriter, "\rremote: Evergit: [Progress] Converting %s: 100%%, done.\n", repoName)
		} else if isIncremental {
			_, _ = fmt.Fprintf(progressWriter, "\rremote: Evergit: [Progress] Converting %s: incremental updates, done.\n", repoName)
		}
	}

	exportErr := <-exportErrChan
	transformErr := <-transformErrChan

	// If fast-export or stream transformation failed with a non-pipe error, report that first
	if exportErr != nil && !isBrokenPipe(exportErr) {
		return fmt.Errorf("git fast-export failed: %v (stderr: %q)", exportErr, strings.TrimSpace(exportErrBuf.String()))
	}
	if transformErr != nil && !errors.Is(transformErr, io.EOF) && !isBrokenPipe(transformErr) {
		return fmt.Errorf("git fast-export stream transform failed: %w", transformErr)
	}
	if importErr != nil {
		return fmt.Errorf("git fast-import failed: %v (stderr: %q)", importErr, strings.TrimSpace(importErrBuf.String()))
	}
	if exportErr != nil {
		return fmt.Errorf("git fast-export failed: %v (stderr: %q)", exportErr, strings.TrimSpace(exportErrBuf.String()))
	}
	if transformErr != nil && !errors.Is(transformErr, io.EOF) {
		return fmt.Errorf("git fast-export stream transform failed: %w", transformErr)
	}
	return nil
}

func isBrokenPipe(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "broken pipe") || strings.Contains(msg, "closed pipe")
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
	"GIT_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_PARAMETERS",
}

// GitEnv returns the process environment without redirecting git variables, plus extra.
// It isolates internal Git processes from user and system gitconfigs (such as url.*.insteadOf
// rules) to prevent recursive proxy loops where Evergit attempts to clone from itself.
func GitEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(redirectingGitEnv, name) {
			env = append(env, kv)
		}
	}
	env = append(env,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	return append(env, extra...)
}

func runCmd(dir, name string, args ...string) (string, error) {
	return runCmdTimeout(0, dir, name, args...)
}

// runCmdTimeout runs a command, killing it after timeout (0 means no limit).
func runCmdTimeout(timeout time.Duration, dir, name string, args ...string) (string, error) {
	return runCmdContext(context.Background(), timeout, dir, name, args...)
}

// runCmdContext runs a command with context cancellation and an optional timeout.
func runCmdContext(ctx context.Context, timeout time.Duration, dir, name string, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
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
	// progressObjectInterval specifies how often fast-export emits progress statements (in objects).
	progressObjectInterval = 5000
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
