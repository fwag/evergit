package converter

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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

// ErrUpstreamObjectFormatChanged is returned when an upstream converted to SHA-1 changed its object
// format, e.g. moved to SHA-256 at the same URL: its mirror can no longer be updated, and serving the
// converted copy as if the upstream were unreachable would hide it.
var ErrUpstreamObjectFormatChanged = errors.New("upstream changed its object format")

// DefaultConversionTimeout is the maximum time a request will wait in the conversion queue before shedding load.
const DefaultConversionTimeout = 30 * time.Second

// DefaultExecTimeout is the maximum time a repository conversion may run before being terminated.
// A first conversion of the Linux kernel takes about 4 hours on 4 CPUs.
const DefaultExecTimeout = 12 * time.Hour

// ErrConversionInProgress is returned by EnsureRepoWithin when a repository without a converted
// copy is still being converted; the conversion continues and a later request may succeed.
var ErrConversionInProgress = errors.New("repository is being converted")

// DefaultMaxSubmoduleDepth is the maximum nesting of submodules converted for a repository (see
// config.DefaultMaxSubmoduleDepth).
const DefaultMaxSubmoduleDepth = config.DefaultMaxSubmoduleDepth

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

// repoJob tracks an active conversion job. It runs to completion (or until the exec timeout) even
// when every client waiting for it has gone, so long conversions finish behind proxies with short
// timeouts instead of being restarted by every request.
type repoJob struct {
	mu   sync.Mutex
	done chan struct{}
	err  error
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

// SetDebug enables or disables debug logging.
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

// beingConverted reports whether a repository is being converted further up the chain of submodules.
func beingConverted(ctx context.Context, canonicalID string) bool {
	visited, _ := ctx.Value(visitedReposKey{}).(map[string]bool)
	return visited[canonicalID]
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

// EnsureRepoWithin is EnsureRepo for clients behind proxies with short timeouts. It waits at most
// the given duration for a running sync or conversion and returns its result if it ends in time.
// Otherwise it returns nil if a converted copy exists, which can be served (a repository being
// updated stays consistent for readers), or ErrConversionInProgress if none does. Either way the
// conversion continues in the background. A duration of 0 waits until the end.
func (m *Manager) EnsureRepoWithin(ctx context.Context, info *resolver.RepositoryInfo, progressWriter io.Writer, wait time.Duration) error {
	if wait <= 0 {
		return m.EnsureRepo(ctx, info, progressWriter)
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	err := m.EnsureRepo(waitCtx, info, progressWriter)
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return err
	}
	if _, statErr := os.Stat(filepath.Join(info.ServingPath, "HEAD")); statErr == nil {
		log.Printf("Serving the current copy of %s while it is being updated", info.RepoName)
		return nil
	}
	return fmt.Errorf("%w: %s", ErrConversionInProgress, info.RepoName)
}

// EnsureRepoWithDepth checks or converts a repository, converting submodules nested at most depth levels.
func (m *Manager) EnsureRepoWithDepth(ctx context.Context, info *resolver.RepositoryInfo, progressWriter io.Writer, depth int) error {
	return m.ensureRepo(ctx, info, progressWriter, depth, nil, false)
}

func canonicalRepoID(info *resolver.RepositoryInfo) string {
	return info.Domain + "/" + info.Path()
}

// ensureRepo checks or converts a repository. A non-empty requiredCommits lists commits the repository
// must contain: the upstream is fetched regardless of the TTL, commits no branch or tag reaches are
// fetched by id, and a fetch failure is returned instead of serving the cached repository. With
// partial, a repository not converted completely before is converted only as far as the history of
// the required commits and those required before (see partialConversion).
func (m *Manager) ensureRepo(ctx context.Context, info *resolver.RepositoryInfo, progressWriter io.Writer, depth int, requiredCommits []string, partial bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	canonicalID := canonicalRepoID(info)
	ctxWithVisited, ok := withVisitedRepo(ctx, canonicalID)
	if !ok {
		// Waiting for the conversion further up the chain would deadlock
		return fmt.Errorf("%w: %s is already being converted further up the chain of submodules", ErrSubmoduleCycle, canonicalID)
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
	for lock.job != nil {
		job := lock.job
		if len(requiredCommits) == 0 {
			lock.mu.Unlock()
			if err := m.waitJob(ctx, job); err != nil || !servedPartially(info.ServingPath) {
				return err
			}
			// The job converted the repository partially, for submodule commits: complete it
			lock.mu.Lock()
			continue
		}
		// A running job may not fetch the required commits: wait for it to exit before starting a new one
		lock.mu.Unlock()
		select {
		case <-job.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		lock.mu.Lock()
	}

	// 1. Check if the repository already exists and is within its TTL
	if len(requiredCommits) == 0 && !m.needsSync(info) {
		lock.mu.Unlock()
		return nil
	}

	job := &repoJob{done: make(chan struct{})}
	timeout := m.execTimeout
	if timeout <= 0 {
		timeout = DefaultExecTimeout
	}
	jobCtx, cancel := context.WithTimeout(context.Background(), timeout)
	// The job outlives this request, but submodule conversions it starts must see the chain
	jobCtx = context.WithValue(jobCtx, visitedReposKey{}, ctx.Value(visitedReposKey{}))
	lock.job = job
	lock.mu.Unlock()

	go func() {
		defer cancel()
		err := m.doEnsureRepo(jobCtx, info, progressWriter, depth, requiredCommits, partial)
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

	return m.waitJob(ctx, job)
}

// waitJob waits for a job or for ctx; the job keeps running when ctx ends.
func (m *Manager) waitJob(ctx context.Context, job *repoJob) error {
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

func (m *Manager) doEnsureRepo(ctx context.Context, info *resolver.RepositoryInfo, progressWriter io.Writer, depth int, requiredCommits []string, partial bool) error {
	// 1. Check if the repository already exists and is within its TTL
	if len(requiredCommits) == 0 && !m.needsSync(info) {
		return nil
	}

	// 2. Clone/Fetch the upstream mirror
	err := m.syncMirror(ctx, info)
	var cached *cacheFallbackError
	if errors.As(err, &cached) && len(requiredCommits) == 0 {
		err = nil // serve the cached repository
	}
	if err != nil {
		return err
	}

	// If the upstream mirror is natively SHA-256, there is nothing to convert
	if isRepoSHA256(info.MirrorPath) {
		return m.syncNativeSHA256(ctx, info, progressWriter)
	}

	if len(requiredCommits) > 0 {
		if _, err := m.fetchCommits(ctx, info, requiredCommits, true); err != nil {
			return err
		}
	}
	partial = partial && len(requiredCommits) > 0 && partialConversion(info.ServingPath)
	var heads []string // nil: the history of all refs
	if partial {
		refs, err := listRefs(info.MirrorPath)
		if err != nil {
			return err
		}
		heads = slices.Sorted(maps.Values(pinnedRefs(refs)))
	}

	// Skip conversion when the serving repo is a complete conversion of the mirror's current refs.
	// Comparing with the last successful conversion, rather than with the mirror before the fetch,
	// retries a conversion that failed after its fetch instead of serving the old version. Touching
	// HEAD restarts the TTL. Anything else, such as a lost translation table, is converted again.
	if convertedUpToDate(info, partial) {
		now := time.Now()
		_ = os.Chtimes(filepath.Join(info.ServingPath, "HEAD"), now, now)
		return nil
	}

	// 3. Convert the submodules whose commits the history records, deciding every gitlink's SHA-256
	gitlinks, err := m.resolveGitlinks(ctx, info, progressWriter, depth, heads)
	if err != nil {
		return err
	}
	defer gitlinks.close()

	// 4. Convert mirror to serving repo (SHA1 -> SHA256 conversion)
	return m.convertRepo(ctx, info, progressWriter, gitlinks, partial)
}

// partialConversion reports whether a repository needed for submodule commits can be converted
// partially: one never converted, or converted partially before. A complete conversion is never
// reduced, which would remove its branches and tags.
func partialConversion(servingPath string) bool {
	if state, err := loadState(servingPath); err == nil {
		return state.Partial
	}
	_, err := os.Stat(filepath.Join(servingPath, "HEAD"))
	return errors.Is(err, os.ErrNotExist)
}

// pinnedPrefix holds the submodule commits that superprojects record (see fetchCommits).
const pinnedPrefix = "refs/evergit-pinned/"

// pinnedRefs returns the refs under pinnedPrefix.
func pinnedRefs(refs map[string]string) map[string]string {
	pinned := make(map[string]string)
	for name, id := range refs {
		if strings.HasPrefix(name, pinnedPrefix) {
			pinned[name] = id
		}
	}
	return pinned
}

// isRepoSHA256 checks if a git repository is configured with the sha256 object format.
func isRepoSHA256(repoPath string) bool {
	format, err := runCmd(repoPath, "git", "--git-dir=.", "rev-parse", "--show-object-format")
	return err == nil && strings.TrimSpace(format) == "sha256"
}

// syncNativeSHA256 creates or updates the serving repository for an upstream repository that is
// already in SHA-256 format, bypassing object conversion.
func (m *Manager) syncNativeSHA256(ctx context.Context, info *resolver.RepositoryInfo, progressWriter io.Writer) error {
	servingHeadPath := filepath.Join(info.ServingPath, "HEAD")
	if _, err := os.Stat(servingHeadPath); err == nil {
		if nativeUpToDate(info) {
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
	mu     sync.Mutex
	closed bool
	lines  chan []byte
	done   chan struct{}
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

// Write never blocks, and drops lines once the client is gone: a conversion outlives the request
// that started it.
func (a *asyncWriter) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return len(p), nil
	}
	select {
	case a.lines <- append([]byte(nil), p...):
	default: // Reader is not keeping up; drop the progress line
	}
	return len(p), nil
}

// Close stops accepting writes and gives queued lines a moment to flush, so progress is not
// interleaved with output written after the conversion. A stalled reader is abandoned.
func (a *asyncWriter) Close() {
	a.mu.Lock()
	a.closed = true
	close(a.lines)
	a.mu.Unlock()
	select {
	case <-a.done:
	case <-time.After(time.Second):
	}
}

// needsSync reports whether a request for the repository itself must sync and convert it.
func (m *Manager) needsSync(info *resolver.RepositoryInfo) bool {
	fi, err := os.Stat(filepath.Join(info.ServingPath, "HEAD"))
	if err != nil {
		return true // Serving repo doesn't exist, must clone and convert
	}
	// A repository converted partially for submodule commits is completed when requested itself
	return servedPartially(info.ServingPath) || time.Since(fi.ModTime()) > m.cacheTTL
}

// servedPartially reports whether the serving repository is a partial conversion.
func servedPartially(servingPath string) bool {
	state, err := loadState(servingPath)
	return err == nil && state.Partial
}

// syncMirror clones or fetches the upstream into the mirror, archiving overwritten and deleted refs.
func (m *Manager) syncMirror(ctx context.Context, info *resolver.RepositoryInfo) error {
	if err := os.MkdirAll(filepath.Dir(info.MirrorPath), 0755); err != nil {
		return fmt.Errorf("failed to create mirrors directory: %w", err)
	}

	_, statErr := os.Stat(info.MirrorPath)
	if statErr == nil && !looksLikeBareRepo(info.MirrorPath) {
		// git would otherwise walk up from this directory and operate on an enclosing repository
		log.Printf("WARNING: mirror %s is not a git repository (interrupted clone?), re-cloning", info.MirrorPath)
		if err := os.RemoveAll(info.MirrorPath); err != nil {
			return fmt.Errorf("failed to remove broken mirror %s: %w", info.MirrorPath, err)
		}
		statErr = os.ErrNotExist
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("failed to access mirror %s: %w", info.MirrorPath, statErr)
	}

	if os.IsNotExist(statErr) {
		if err := m.checkDiskSpace(info.MirrorPath); err != nil {
			return err
		}
		// Clone as a bare mirror
		// --bare rather than --mirror: only branches and tags, not forge refs such as GitHub's
		// thousands of refs/pull/* from forks
		err := m.gitUpstream(ctx, info, "", func(remote string) []string {
			return []string{"-c", "http.followRedirects=false", "clone", "--bare", "--", remote, info.MirrorPath}
		})
		if err != nil {
			_ = os.RemoveAll(info.MirrorPath)
			return fmt.Errorf("failed to clone remote %s: %w", info.RemoteURL, markGone(err))
		}
		return nil
	} else {
		// Read current refs before fetching: without this baseline, force-pushes cannot be archived
		oldRefs, err := listRefs(info.MirrorPath)
		if err != nil {
			if _, statErr := os.Stat(filepath.Join(info.ServingPath, "HEAD")); statErr == nil {
				log.Printf("WARNING: cannot read refs of mirror %s (%v), skipping upstream fetch and serving cached repository", info.MirrorPath, err)
				return &cacheFallbackError{err}
			}
			return fmt.Errorf("failed to read refs of mirror %s: %w", info.MirrorPath, err)
		}

		// Fetch updates
		err = m.gitUpstream(ctx, info, info.MirrorPath, func(remote string) []string {
			return append([]string{"-c", "http.followRedirects=false", "fetch", "--prune", remote}, mirrorRefspecs...)
		})
		if err != nil && strings.Contains(err.Error(), "mismatched algorithms") {
			// Removing the mirror serves the upstream's own history natively, whose commit ids may
			// differ from those served so far
			log.Printf("ERROR: upstream %s changed its object format; mirror %s cannot be updated, remove it to serve the upstream natively: %v", info.RemoteURL, info.MirrorPath, err)
			return fmt.Errorf("%w: %s no longer has the object format of the converted copy; ask the Evergit administrator", ErrUpstreamObjectFormatChanged, info.RemoteURL)
		}

		// Archive overwritten/deleted refs even if the fetch failed midway, since it may already
		// have pruned or updated some of them
		if len(oldRefs) > 0 {
			m.backupForcePushedRefs(info, oldRefs)
		}

		if err != nil {
			// Check if we have an existing serving repository to fall back to
			if _, statErr := os.Stat(filepath.Join(info.ServingPath, "HEAD")); statErr == nil {
				log.Printf("WARNING: Upstream fetch failed for %s (%v). Falling back to cached repository.", info.RemoteURL, err)
				return &cacheFallbackError{markGone(err)}
			}
			return fmt.Errorf("upstream fetch failed and no local cache available for %s: %w", info.RemoteURL, markGone(err))
		}
		return nil
	}
}

// convertedUpToDate reports whether the serving repository is a complete conversion of the
// mirror's current refs, or with partial, of its current pinned refs.
func convertedUpToDate(info *resolver.RepositoryInfo, partial bool) bool {
	state := currentState(info.ServingPath)
	if state == nil || state.Partial != partial || !isIncrementalRewrite(info.ServingPath) {
		return false
	}
	refs, err := listRefs(info.MirrorPath)
	if partial {
		refs = pinnedRefs(refs)
	}
	return err == nil && maps.Equal(refs, state.ConvertedRefs)
}

// nativeUpToDate reports whether a natively SHA-256 serving repository has the mirror's current
// branches, tags and backups. Backups the mirror no longer has are kept by the serving repository.
func nativeUpToDate(info *resolver.RepositoryInfo) bool {
	mirrorRefs, err := listRefs(info.MirrorPath)
	if err != nil {
		return false
	}
	servingRefs, err := listRefs(info.ServingPath)
	if err != nil {
		return false
	}
	for ref, id := range mirrorRefs {
		if servingRefs[ref] != id {
			return false
		}
	}
	for ref := range servingRefs {
		if _, ok := mirrorRefs[ref]; !ok && !strings.HasPrefix(ref, "refs/evergit-backups/") {
			return false
		}
	}
	return true
}

// cacheFallbackError reports that the upstream could not be fetched but a converted repository
// exists, which can be served as cached.
type cacheFallbackError struct {
	err error
}

func (e *cacheFallbackError) Error() string {
	return "upstream unavailable, cached repository can be served: " + e.err.Error()
}

func (e *cacheFallbackError) Unwrap() error {
	return e.err
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

func (m *Manager) convertRepo(ctx context.Context, info *resolver.RepositoryInfo, progressWriter io.Writer, gitlinks *gitlinkResolution, partial bool) error {
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

	return m.convertObjects(ctx, info, progressWriter, gitlinks, partial)
}

// syntheticSHA256Padding turns the SHA-1 of a submodule commit without a known translation into a
// deterministic placeholder SHA-256.
const syntheticSHA256Padding = "000000000000000000000000" // 24 zeros

// syntheticID returns the synthetic SHA-256 of a submodule commit: its SHA-1 followed by zeros.
func syntheticID(id sha1ID) sha256ID {
	var newID sha256ID
	copy(newID[:], id[:])
	return newID
}

// publishConversion sets HEAD like the mirror's, restarts the TTL and, for a full conversion,
// atomically publishes the new build.
func (m *Manager) publishConversion(info *resolver.RepositoryInfo, targetPath string, isIncremental bool) error {
	headRef, err := runCmd(info.MirrorPath, "git", "symbolic-ref", "HEAD")
	if err != nil {
		headRef = "refs/heads/main"
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
	return runCmdStdin(ctx, timeout, dir, nil, name, args...)
}

// runCmdStdin is runCmdContext feeding stdin to the command.
func runCmdStdin(ctx context.Context, timeout time.Duration, dir string, stdin io.Reader, name string, args ...string) (string, error) {
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
	cmd.Stdin = stdin
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
	// progressObjectInterval specifies how often conversion progress is reported (in objects).
	progressObjectInterval = 5000
)

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

	index, _ := openTranslationIndex(filepath.Join(info.ServingPath, sha256IndexFile), len(sha256ID{}))
	defer index.Close()
	for refName, sha256 := range backups {
		sha1 := "unknown"
		if key, err := hex.DecodeString(sha256); err == nil {
			if value, ok := index.lookup(key); ok {
				sha1 = hex.EncodeToString(value)
			}
		}
		log.Printf("[Backup] Preserved history: %s -> SHA256: %s (SHA1: %s)", refName, sha256, sha1)
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

	// Binary search in the translation index; repositories converted before it existed are scanned
	indexFile := map[string]string{"sha1": sha1IndexFile, "sha256": sha256IndexFile}[format]
	if index, err := openTranslationIndex(filepath.Join(repoPath, indexFile), maxLen/2); err == nil {
		defer index.Close()
		switch matches := index.withPrefix(id, 2); len(matches) {
		case 0:
			return "", fmt.Errorf("%w: %s", ErrIDNotFound, id)
		case 1:
			return hex.EncodeToString(matches[0]), nil
		default:
			return "", fmt.Errorf("%w: %s", ErrIDAmbiguous, id)
		}
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
