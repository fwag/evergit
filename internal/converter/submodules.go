package converter

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"evergit/internal/config"
	"evergit/internal/resolver"
)

// A gitlink records a commit of another repository. Its SHA-256 must be the same in every
// conversion of the same history, so it is decided from the history itself: the submodule URL comes
// from the .gitmodules of the commit recording the gitlink, and the submodule is converted. When
// that is impossible (no URL, host not allowed, unusable URL, repository or commit gone upstream),
// the synthetic SHA-256 is only used if the administrator approved it (config.SyntheticGitlink);
// otherwise, as for temporary failures, the conversion fails. Decisions are recorded so later
// conversions of the same repository never translate a submodule commit differently.

// ErrSubmoduleCycle is returned when converting a repository requires converting a repository that
// is already being converted further up the same chain of submodules.
var ErrSubmoduleCycle = errors.New("submodule cycle")

// ErrSubmoduleDepth is returned when submodules are nested deeper than the configured limit.
var ErrSubmoduleDepth = errors.New("submodules nested too deeply")

// ErrSubmoduleNotApproved is returned when submodule commits cannot be converted for good and the
// administrator has not approved recording them with the synthetic SHA-256.
var ErrSubmoduleNotApproved = errors.New("submodule needs administrator approval")

// ErrSubmoduleUnavailable is returned when a submodule could not be converted for a reason that may
// be temporary (busy, unreachable upstream, ...). The conversion fails rather than recording the
// gitlink with the synthetic SHA-256, and a later attempt may succeed.
var ErrSubmoduleUnavailable = errors.New("submodule temporarily unavailable")

// errUpstreamGone marks a repository or commit that does not exist upstream: a permanent reason to
// record a gitlink with the synthetic SHA-256.
var errUpstreamGone = errors.New("gone upstream")

// goneMessages are git errors for a repository or commit the upstream does not have. Forges answer
// a request for a missing repository like one for a private repository, by asking for credentials,
// and Evergit never has any: either is unavailable for good. Network errors are not listed.
var goneMessages = []string{
	"Authentication failed for '", // missing or private repository on GitHub, GitLab, ...
	"Repository not found",        // GitHub
	"' not found",                 // HTTP 404: repository '<url>' not found
	"' does not exist",            // local path: repository '<path>' does not exist
	"not our ref",                 // commit unknown to the server
	"unadvertised object",
	"couldn't find remote ref",
	"no such remote ref",
}

// markGone wraps err with errUpstreamGone when git reports a missing repository or commit.
func markGone(err error) error {
	if err == nil {
		return nil
	}
	gone := false
	for _, m := range goneMessages {
		gone = gone || strings.Contains(err.Error(), m)
	}
	if gone {
		return fmt.Errorf("%w: %w", errUpstreamGone, err)
	}
	return err
}

// gitlinksFile records the SHA-256 decided for every submodule commit of a serving repository, in
// the format of loose-object-idx.
const gitlinksFile = "evergit-gitlinks"

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

// gitlinkTargets maps a submodule to the submodule commits recorded for it. A submodule is the URL
// the recording commit's .gitmodules gives, or "path:<path>" when it gives none, as in
// config.SyntheticGitlink.
type gitlinkTargets map[string]map[string]bool

// pathPrefix identifies a gitlink without URL by its path.
const pathPrefix = "path:"

// discoverGitlinks lists the submodule commits recorded by the commits reachable from the mirror's
// refs but not from exclude, by submodule.
func discoverGitlinks(ctx context.Context, mirrorPath string, exclude []string) (gitlinkTargets, error) {
	// Merges are diffed against all parents (-c), so a gitlink a merge introduces is listed too
	args := []string{"log", "--all", "--root", "-c", "-r", "--raw", "--no-abbrev", "--no-renames", "--format=commit %H"}
	if len(exclude) > 0 {
		args = append(append(args, "--not"), exclude...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = mirrorPath
	cmd.Env = GitEnv()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	type gitlink struct{ path, target string }
	recorded := make(map[string][]gitlink)
	var commits []string
	commit := ""
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if c, ok := strings.CutPrefix(line, "commit "); ok {
			commit = c
			continue
		}
		meta, p, ok := strings.Cut(line, "\t")
		if !ok || !strings.HasPrefix(meta, ":") {
			continue
		}
		// ":<modes...> <ids...> <status>", one mode and id per parent plus the result
		parents := len(meta) - len(strings.TrimLeft(meta, ":"))
		fields := strings.Fields(meta[parents:])
		if len(fields) != 2*(parents+1)+1 || fields[parents] != "160000" {
			continue
		}
		if unquoted, err := strconv.Unquote(p); err == nil && strings.HasPrefix(p, "\"") {
			p = unquoted
		}
		if _, seen := recorded[commit]; !seen {
			commits = append(commits, commit)
		}
		recorded[commit] = append(recorded[commit], gitlink{path: p, target: fields[2*parents+1]})
	}
	if err := scanner.Err(); err != nil {
		_ = cmd.Wait()
		return nil, err
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("git log failed in %s: %w (stderr: %q)", mirrorPath, err, strings.TrimSpace(stderr.String()))
	}

	urls, err := gitmodulesURLs(ctx, mirrorPath, commits)
	if err != nil {
		return nil, err
	}
	targets := make(gitlinkTargets)
	for _, c := range commits {
		for _, g := range recorded[c] {
			u := urls[c][g.path]
			if u == "" {
				u = pathPrefix + g.path
			}
			if targets[u] == nil {
				targets[u] = make(map[string]bool)
			}
			targets[u][g.target] = true
		}
	}
	return targets, nil
}

// gitmodulesURLs returns the submodule path -> URL mapping of each commit's .gitmodules, parsing
// each distinct .gitmodules once.
func gitmodulesURLs(ctx context.Context, mirrorPath string, commits []string) (map[string]map[string]string, error) {
	if len(commits) == 0 {
		return nil, nil
	}
	var input strings.Builder
	for _, c := range commits {
		fmt.Fprintf(&input, "%s:.gitmodules\n", c)
	}
	cmd := exec.CommandContext(ctx, "git", "cat-file", "--batch-check=%(objectname) %(objecttype)")
	cmd.Dir = mirrorPath
	cmd.Env = GitEnv()
	cmd.Stdin = strings.NewReader(input.String())
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file failed in %s: %w", mirrorPath, err)
	}

	byBlob := make(map[string]map[string]string)
	urls := make(map[string]map[string]string, len(commits))
	for i, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		blob, typ, _ := strings.Cut(line, " ")
		if i >= len(commits) || typ != "blob" {
			continue // no .gitmodules in this commit
		}
		if _, parsed := byBlob[blob]; !parsed {
			paths := make(map[string]string)
			if list, err := runCmdContext(ctx, 0, mirrorPath, "git", "config", "--blob", blob, "--list"); err == nil {
				for _, entry := range parseGitmodulesConfig(list) {
					paths[entry.Path] = entry.URL
				}
			}
			byBlob[blob] = paths
		}
		urls[commits[i]] = byBlob[blob]
	}
	return urls, nil
}

// convertedTips returns the SHA-1 ids of the serving repository's refs, whose history has already
// been converted.
func convertedTips(servingPath string) ([]string, error) {
	out, err := runCmd(servingPath, "git", "--git-dir=.", "for-each-ref", "--format=%(objectname)")
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]bool)
	for _, id := range strings.Fields(out) {
		wanted[id] = true
	}
	f, err := os.Open(filepath.Join(servingPath, "objects", "loose-object-idx"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var tips []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if newID, oldID, found := strings.Cut(scanner.Text(), " "); found && wanted[newID] {
			tips = append(tips, oldID)
		}
	}
	return tips, scanner.Err()
}

// gitlinkResolution decides the SHA-256 recorded for each submodule commit, in order of precedence:
// a decision recorded by an earlier conversion, the submodules' translation tables, the synthetic
// SHA-256 for commits with a permanent reason, and otherwise the repository's own objects (a
// submodule pointing into its own repository).
type gitlinkResolution struct {
	decided map[sha1ID]sha256ID // extended with every decision of this conversion
	tables  []map[string]string // sha1 -> sha256 of converted submodules
	padded  map[string]bool
}

func newGitlinkResolution(decided map[sha1ID]sha256ID) *gitlinkResolution {
	if decided == nil {
		decided = make(map[sha1ID]sha256ID)
	}
	return &gitlinkResolution{decided: decided, padded: make(map[string]bool)}
}

// resolve returns the SHA-256 for a submodule commit, or ok=false if it must be one of the
// repository's own commits.
func (g *gitlinkResolution) resolve(id sha1ID) (newID sha256ID, ok bool, err error) {
	if newID, ok := g.decided[id]; ok {
		return newID, true, nil
	}
	key := hex.EncodeToString(id[:])
	found := ""
	for _, table := range g.tables {
		if v, ok := table[key]; ok && len(v) == sha256HexLen {
			if found != "" && found != v {
				return sha256ID{}, false, fmt.Errorf("submodule commit %s has conflicting SHA-256 translations %s and %s", key, found, v)
			}
			found = v
		}
	}
	if found == "" && g.padded[key] {
		found = key + syntheticSHA256Padding
	}
	if found == "" {
		return sha256ID{}, false, nil
	}
	if _, err := hex.Decode(newID[:], []byte(found)); err != nil {
		return sha256ID{}, false, err
	}
	return newID, true, nil
}

// resolveGitlinks converts the submodules of the commits the conversion will add and decides the
// SHA-256 of every submodule commit they record. Earlier decisions are kept, also across rebuilds.
func (m *Manager) resolveGitlinks(ctx context.Context, info *resolver.RepositoryInfo, progressWriter io.Writer, depth int) (*gitlinkResolution, error) {
	decided, err := loadTranslationFile(filepath.Join(info.ServingPath, gitlinksFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("failed to load recorded gitlinks: %w", err)
	}
	res := newGitlinkResolution(decided)

	var exclude []string
	if isIncrementalRewrite(info.ServingPath) {
		if exclude, err = convertedTips(info.ServingPath); err != nil {
			return nil, err
		}
	}
	targets, err := discoverGitlinks(ctx, info.MirrorPath, exclude)
	if err != nil {
		return nil, err
	}

	// A missing file approves nothing, so it can be configured before the first approval
	approvals, err := config.LoadSyntheticGitlinks(m.syntheticGitlinksFile())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	superproject := canonicalRepoID(info)
	// Commits that cannot be converted, checked once all submodules are converted: another
	// submodule (e.g. the same one at another path) may still translate them
	type problem struct {
		submodule, reason string
		commits           []string
	}
	var problems []problem

	submodules := make([]string, 0, len(targets))
	for submodule := range targets {
		submodules = append(submodules, submodule)
	}
	slices.Sort(submodules)
	for _, submodule := range submodules {
		var pending []string
		for target := range targets[submodule] {
			if id, err := parseSHA1Hex(target); err == nil {
				if _, ok := res.decided[id]; !ok {
					pending = append(pending, target)
				}
			}
		}
		if len(pending) == 0 {
			continue
		}
		slices.Sort(pending)

		synthetic := func(commits []string, reason string) {
			problems = append(problems, problem{submodule: submodule, reason: reason, commits: commits})
		}
		if strings.HasPrefix(submodule, pathPrefix) {
			synthetic(pending, "no URL in the recording commit's .gitmodules")
			continue
		}
		subInfo, err := m.resolveSubmoduleInfo(submodule, info)
		if err != nil {
			synthetic(pending, "URL cannot be mirrored: "+err.Error())
			continue
		}
		if canonicalRepoID(subInfo) == superproject {
			// Translated from the repository's own objects, if it has them
			missing, err := missingCommits(ctx, info.MirrorPath, pending)
			if err != nil {
				return nil, err
			}
			if len(missing) > 0 {
				synthetic(missing, "commit not in the repository itself")
			}
			continue
		}
		if depth <= 0 {
			return nil, fmt.Errorf("%w: %s needs submodule %s", ErrSubmoduleDepth, info.RepoName, submodule)
		}

		table, gone, err := m.ensureSubmoduleCommits(ctx, subInfo, pending, progressWriter, depth-1)
		if errors.Is(err, ErrSubmoduleCycle) || errors.Is(err, ErrSubmoduleDepth) || errors.Is(err, ErrSubmoduleUnavailable) || errors.Is(err, ErrSubmoduleNotApproved) {
			return nil, fmt.Errorf("submodule %s of %s: %w", submodule, info.RepoName, err)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: submodule %s of %s: %w", ErrSubmoduleUnavailable, submodule, info.RepoName, err)
		}
		if len(gone) > 0 {
			synthetic(gone, "commit or repository gone upstream")
		}
		res.tables = append(res.tables, table)
	}

	// The synthetic SHA-256 is only recorded if the administrator approved it
	var unapproved, unapprovedReasons []string
	for _, p := range problems {
		var remaining []string
		for _, c := range p.commits {
			if id, err := parseSHA1Hex(c); err == nil {
				if _, ok, err := res.resolve(id); err == nil && !ok {
					remaining = append(remaining, c)
				}
			}
		}
		if len(remaining) == 0 {
			continue
		}
		if !config.ApprovesSynthetic(approvals, superproject, p.submodule) {
			unapproved = append(unapproved, p.submodule)
			unapprovedReasons = append(unapprovedReasons, fmt.Sprintf("  %s: %s (%d commit(s))\n", p.submodule, p.reason, len(remaining)))
			continue
		}
		log.Printf("[%s] Recording %d commit(s) of submodule %q with the synthetic SHA-256 as approved: %s", info.RepoName, len(remaining), p.submodule, p.reason)
		for _, c := range remaining {
			res.padded[c] = true
		}
	}
	if len(unapproved) > 0 {
		var hint strings.Builder
		for _, submodule := range unapproved {
			fmt.Fprintf(&hint, "  %s %s\n", superproject, submodule)
		}
		return nil, fmt.Errorf("%w: submodules of %s cannot be converted:\n%s\nAsk the Evergit administrator to allow their hosts (-allowed-hosts), or to approve synthetic SHA-256s for them in the synthetic gitlinks file (-synthetic-gitlinks):\n%s",
			ErrSubmoduleNotApproved, superproject, strings.Join(unapprovedReasons, ""), hint.String())
	}
	return res, nil
}

// syntheticGitlinksFile is the administrator's list of submodules approved for synthetic SHA-256s.
func (m *Manager) syntheticGitlinksFile() string {
	if m.cfg == nil {
		return ""
	}
	return m.cfg.SyntheticGitlinksFile
}

// ensureSubmoduleCommits converts a submodule so that its translation table covers commits, and
// returns the table and the commits that do not exist upstream.
func (m *Manager) ensureSubmoduleCommits(ctx context.Context, subInfo *resolver.RepositoryInfo, commits []string, progressWriter io.Writer, depth int) (map[string]string, []string, error) {
	missingFrom := func() (map[string]string, []string) {
		table, _ := loadSubmoduleTranslations(subInfo.ServingPath)
		var missing []string
		for _, c := range commits {
			if _, ok := table[c]; !ok {
				missing = append(missing, c)
			}
		}
		return table, missing
	}

	err := m.ensureRepo(ctx, subInfo, progressWriter, depth, nil)
	if errors.Is(err, errUpstreamGone) {
		return nil, commits, nil
	}
	if err != nil {
		return nil, nil, err
	}
	table, missing := missingFrom()
	if len(missing) == 0 {
		return table, nil, nil
	}

	// Refresh regardless of the TTL, fetching commits that no branch or tag reaches
	err = m.ensureRepo(ctx, subInfo, progressWriter, depth, missing)
	if err != nil && !errors.Is(err, errUpstreamGone) {
		return nil, nil, err
	}
	table, missing = missingFrom()
	return table, missing, nil
}

// pinCommits makes wanted commits part of the mirror's history under refs/evergit-pinned/, fetching
// them by id when the mirror lacks them, and reports whether any was pinned. Commits the upstream
// does not have are skipped; other failures are returned.
func pinCommits(ctx context.Context, info *resolver.RepositoryInfo, want []string) (bool, error) {
	pinned := false
	for _, c := range want {
		if _, err := parseSHA1Hex(c); err != nil {
			return pinned, err
		}
		ref := "refs/evergit-pinned/" + c
		if _, err := runCmdContext(ctx, 0, info.MirrorPath, "git", "cat-file", "-e", c+"^{commit}"); err == nil {
			if _, err := runCmdContext(ctx, 0, info.MirrorPath, "git", "update-ref", ref, c); err != nil {
				return pinned, err
			}
		} else if _, err := runCmdContext(ctx, upstreamTimeout, info.MirrorPath, "git", "-c", "http.followRedirects=false", "fetch", "--no-tags", "origin", "+"+c+":"+ref); err != nil {
			if err := markGone(err); errors.Is(err, errUpstreamGone) {
				log.Printf("[%s] Submodule commit %s does not exist upstream: %v", info.RepoName, c, err)
				continue
			}
			return pinned, fmt.Errorf("failed to fetch submodule commit %s: %w", c, err)
		}
		pinned = true
	}
	return pinned, nil
}

// missingCommits returns the commits the repository does not contain.
func missingCommits(ctx context.Context, repoPath string, commits []string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "cat-file", "--batch-check=%(objecttype)")
	cmd.Dir = repoPath
	cmd.Env = GitEnv()
	cmd.Stdin = strings.NewReader(strings.Join(commits, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file failed in %s: %w", repoPath, err)
	}
	var missing []string
	for i, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		if i < len(commits) && line != "commit" {
			missing = append(missing, commits[i])
		}
	}
	return missing, nil
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

// loadSubmoduleTranslations reads a submodule's SHA-1 -> SHA-256 translations from its loose-object-idx.
func loadSubmoduleTranslations(servingPath string) (map[string]string, error) {
	f, err := os.Open(filepath.Join(servingPath, "objects", "loose-object-idx"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sha1ToSHA256 := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		sha256, sha1, found := strings.Cut(scanner.Text(), " ")
		if found && len(sha256) == sha256HexLen && len(sha1) == sha1HexLen {
			sha1ToSHA256[sha1] = sha256
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return sha1ToSHA256, nil
}
