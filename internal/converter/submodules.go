package converter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/url"
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
// the commit is translated from another URL the history records it under, or another URL of the
// same path; failing that, the synthetic SHA-256 is only used if the administrator approved it
// (config.SyntheticGitlink); otherwise, as for temporary failures, the conversion fails. Decisions
// are recorded so later conversions of the same repository never translate a submodule commit
// differently.

// ErrTooManySubmodules is returned when a conversion needs more submodule repositories than the
// configured limit.
var ErrTooManySubmodules = errors.New("too many submodule repositories")

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

// progressCommitInterval specifies how often submodule discovery progress is reported (in commits).
const progressCommitInterval = 1000

// discoverGitlinks lists the submodule commits recorded by the commits reachable from heads (all of
// the mirror's refs if nil) but not from exclude, by submodule.
func discoverGitlinks(ctx context.Context, mirrorPath string, heads, exclude []string, progress func(n int)) (gitlinkTargets, error) {
	// Merges are diffed against all parents (-c), so a gitlink a merge introduces is listed too
	args, stdin := historyRevisions(heads, exclude)
	cmd := exec.CommandContext(ctx, "git", append([]string{"log", "--root", "-c", "-r", "--raw", "--no-abbrev", "--no-renames", "--format=commit %H"}, args...)...)
	cmd.Dir = mirrorPath
	cmd.Stdin = stdin
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
	scanned := 0
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if c, ok := strings.CutPrefix(line, "commit "); ok {
			commit = c
			scanned++
			if progress != nil && scanned%progressCommitInterval == 0 {
				progress(scanned)
			}
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
		if strings.HasPrefix(p, "\"") {
			if unquoted, err := strconv.Unquote(p); err == nil {
				p = unquoted
			}
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
	if progress != nil && scanned > 0 {
		progress(scanned)
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

// submoduleAliases returns, for each submodule URL in the history of the mirror's refs, the other
// URLs the same path had, e.g. after the submodule moved. A gitlink without URL ("path:<path>") gets
// those of its path.
func submoduleAliases(ctx context.Context, mirrorPath string) (map[string][]string, error) {
	out, err := runCmdContext(ctx, 0, mirrorPath, "git", "log", "--all", "--full-history", "--format=%H", "--", ".gitmodules")
	if err != nil {
		return nil, err
	}
	urls, err := gitmodulesURLs(ctx, mirrorPath, strings.Fields(out))
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]map[string]bool)
	for _, paths := range urls {
		for p, u := range paths {
			if byPath[p] == nil {
				byPath[p] = make(map[string]bool)
			}
			byPath[p][u] = true
		}
	}
	aliases := make(map[string][]string)
	for p, set := range byPath {
		list := slices.Sorted(maps.Keys(set))
		aliases[pathPrefix+p] = list
		for _, u := range list {
			for _, other := range list {
				if other != u && !slices.Contains(aliases[u], other) {
					aliases[u] = append(aliases[u], other)
				}
			}
		}
	}
	for u := range aliases {
		slices.Sort(aliases[u])
	}
	return aliases, nil
}

// gitlinkResolution holds the SHA-256 decided for each submodule commit of a conversion: recorded
// by an earlier conversion or decided by the plan. A commit without a decision is one of the
// repository's own commits (a submodule pointing into its own repository), converted with it.
type gitlinkResolution struct {
	decided map[sha1ID]sha256ID // extended with every decision of this conversion

	// For the conversion state: the commits each submodule records that this conversion decides,
	// and why the synthetic ones were approved
	recorded  map[string][]string
	synthetic map[string]syntheticDecision
}

func newGitlinkResolution(decided map[sha1ID]sha256ID) *gitlinkResolution {
	if decided == nil {
		decided = make(map[sha1ID]sha256ID)
	}
	return &gitlinkResolution{
		decided:   decided,
		recorded:  make(map[string][]string),
		synthetic: make(map[string]syntheticDecision),
	}
}

// resolve returns the SHA-256 for a submodule commit, or ok=false if it must be one of the
// repository's own commits.
func (g *gitlinkResolution) resolve(id sha1ID) (newID sha256ID, ok bool, err error) {
	newID, ok = g.decided[id]
	return newID, ok, nil
}

// syntheticGitlinksFile is the administrator's list of submodules approved for synthetic SHA-256s.
func (m *Manager) syntheticGitlinksFile() string {
	if m.cfg == nil {
		return ""
	}
	return m.cfg.SyntheticGitlinksFile
}

// fetchCommits makes commits available in the mirror: those it lacks are fetched by id, as no
// branch or tag may reach them, and with pin all of them are kept under pinnedPrefix. It returns the
// commits the upstream does not have; other failures are errors.
func (m *Manager) fetchCommits(ctx context.Context, info *resolver.RepositoryInfo, commits []string, pin bool) ([]string, error) {
	for _, c := range commits {
		if _, err := parseSHA1Hex(c); err != nil {
			return nil, err
		}
	}
	missing, err := missingCommits(ctx, info.MirrorPath, commits)
	if err != nil {
		return nil, err
	}
	var gone []string
	for _, c := range missing {
		err := m.gitUpstream(ctx, info, info.MirrorPath, func(remote string) []string {
			return []string{"-c", "http.followRedirects=false", "fetch", "--no-tags", "--no-write-fetch-head", remote, c}
		})
		if err := markGone(err); errors.Is(err, errUpstreamGone) {
			log.Printf("[%s] Submodule commit %s does not exist upstream: %v", info.RepoName, c, err)
			gone = append(gone, c)
		} else if err != nil {
			return nil, fmt.Errorf("failed to fetch submodule commit %s: %w", c, err)
		}
	}
	if !pin {
		return gone, nil
	}
	var updates strings.Builder
	for _, c := range commits {
		if !slices.Contains(gone, c) {
			fmt.Fprintf(&updates, "update %s%s %s\n", pinnedPrefix, c, c)
		}
	}
	if updates.Len() > 0 {
		if _, err := runCmdStdin(ctx, 0, info.MirrorPath, strings.NewReader(updates.String()), "git", "update-ref", "--stdin"); err != nil {
			return nil, err
		}
	}
	return gone, nil
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

	// Submodules are mirrored from allowed hosts only: a URL in a public repository's .gitmodules
	// must never make Evergit read the server's own files
	if strings.HasPrefix(rawURL, "/") {
		return nil, fmt.Errorf("local path %q is not mirrored", rawURL)
	}

	cfg := m.cfg
	if cfg == nil {
		cfg = &config.Config{
			StorageRoot:  storageRoot,
			AllowedHosts: config.DefaultAllowedHosts,
			Shorthands:   config.ParseShorthands(config.DefaultShorthands),
		}
	}

	var targetPath string
	if strings.HasPrefix(rawURL, "./") || strings.HasPrefix(rawURL, "../") {
		// Relative to the superproject's URL, as git resolves it: "../other.git" next to
		// "<domain>/<owner>/<repo>.git" is "<domain>/<owner>/other.git"
		parentURL := path.Join(parentInfo.Domain, parentInfo.Path()+".git")
		targetPath = path.Clean(path.Join(parentURL, rawURL))
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
		if !slices.Contains(networkSchemes, u.Scheme) {
			return nil, fmt.Errorf("URL scheme %q is not mirrored", u.Scheme)
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

// networkSchemes are the URL schemes of submodules that can be mirrored from an allowed host.
var networkSchemes = []string{"https", "http", "ssh", "git", "git+ssh", "ssh+git"}
