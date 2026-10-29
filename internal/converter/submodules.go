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
// the commit is translated from another URL the history records it under, or another URL of the
// same path; failing that, the synthetic SHA-256 is only used if the administrator approved it
// (config.SyntheticGitlink); otherwise, as for temporary failures, the conversion fails. Decisions
// are recorded so later conversions of the same repository never translate a submodule commit
// differently.

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

// gitlinkResolution decides the SHA-256 recorded for each submodule commit, in order of precedence:
// a decision recorded by an earlier conversion or made by this one (including the synthetic SHA-256
// for commits with a permanent reason), the submodules' translation tables, and otherwise the
// repository's own objects (a submodule pointing into its own repository).
type gitlinkResolution struct {
	decided map[sha1ID]sha256ID // extended with every decision of this conversion
	tables  []*translationIndex // SHA-1 indexes of converted submodules

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
	if newID, ok := g.decided[id]; ok {
		return newID, true, nil
	}
	key := hex.EncodeToString(id[:])
	found := ""
	for _, table := range g.tables {
		if value, ok := table.lookup(id[:]); ok {
			v := hex.EncodeToString(value)
			if found != "" && found != v {
				return sha256ID{}, false, fmt.Errorf("submodule commit %s has conflicting SHA-256 translations %s and %s", key, found, v)
			}
			found = v
		}
	}
	if found == "" {
		return sha256ID{}, false, nil
	}
	if _, err := hex.Decode(newID[:], []byte(found)); err != nil {
		return sha256ID{}, false, err
	}
	return newID, true, nil
}

// close releases the submodules' indexes.
func (g *gitlinkResolution) close() {
	for _, table := range g.tables {
		_ = table.Close()
	}
}

// resolveGitlinks converts the submodules of the commits the conversion will add, those reachable
// from heads (all of the mirror's refs if nil), and decides the SHA-256 of every submodule commit
// they record. Earlier decisions are kept, also across rebuilds.
func (m *Manager) resolveGitlinks(ctx context.Context, info *resolver.RepositoryInfo, progressWriter io.Writer, depth int, heads []string) (_ *gitlinkResolution, err error) {
	var decided map[sha1ID]sha256ID
	if previous, err := loadState(info.ServingPath); err == nil {
		if decided, err = previous.gitlinkDecisions(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	res := newGitlinkResolution(decided)
	defer func() {
		if err != nil {
			res.close()
		}
	}()

	// Gitlinks of history converted before are decided already
	var exclude []string
	if state := currentState(info.ServingPath); state != nil && isIncrementalRewrite(info.ServingPath) {
		for _, id := range state.ConvertedRefs {
			exclude = append(exclude, id)
		}
	}
	if progressWriter != nil {
		_, _ = fmt.Fprintf(progressWriter, "remote: Evergit: [%s] Scanning history for submodules...\n", info.RepoName)
	}
	var scannedCommits int
	progress := func(n int) {
		scannedCommits = n
		if progressWriter != nil {
			_, _ = fmt.Fprintf(progressWriter, "\rremote: Evergit: [Progress] Scanning %s: %d commits...", info.RepoName, n)
		}
	}
	targets, err := discoverGitlinks(ctx, info.MirrorPath, heads, exclude, progress)
	if err != nil {
		return nil, err
	}
	if progressWriter != nil && scannedCommits > 0 {
		_, _ = fmt.Fprintf(progressWriter, "\rremote: Evergit: [Progress] Scanning %s: %d commits, done.\n", info.RepoName, scannedCommits)
	}

	// A missing file approves nothing, so it can be configured before the first approval
	approvals, err := config.LoadSyntheticGitlinks(m.syntheticGitlinksFile())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	superproject := canonicalRepoID(info)
	// How each submodule converted for this repository translates commits
	type translator struct {
		tables     []*translationIndex
		translated map[sha1ID]sha256ID
		own        bool // the repository itself, whose commits are converted with it
	}
	translators := make(map[string]*translator)
	// lookup returns how a submodule translates a commit: its SHA-256, or own for one of the
	// repository's own commits
	lookup := func(submodule string, id sha1ID) (newID sha256ID, own, ok bool) {
		t := translators[submodule]
		if t == nil {
			return sha256ID{}, false, false
		}
		if newID, ok := t.translated[id]; ok {
			return newID, false, true
		}
		for _, table := range t.tables {
			if value, ok := table.lookup(id[:]); ok {
				copy(newID[:], value)
				return newID, false, true
			}
		}
		if t.own {
			missing, err := missingCommits(ctx, info.MirrorPath, []string{hex.EncodeToString(id[:])})
			return sha256ID{}, true, err == nil && len(missing) == 0
		}
		return sha256ID{}, false, false
	}

	// convertSubmodule converts commits of a submodule, and returns those that cannot be converted
	// and why
	convertSubmodule := func(submodule string, commits []string) (failed []string, reason string, err error) {
		if strings.HasPrefix(submodule, pathPrefix) {
			return commits, "no URL in the recording commit's .gitmodules", nil
		}
		subInfo, err := m.resolveSubmoduleInfo(submodule, info)
		if err != nil {
			return commits, "URL cannot be mirrored: " + err.Error(), nil
		}
		t := translators[submodule]
		if t == nil {
			t = &translator{translated: make(map[sha1ID]sha256ID)}
			translators[submodule] = t
		}
		if canonicalRepoID(subInfo) == superproject {
			// Translated from the repository's own objects; those it lacks are fetched from upstream
			t.own = true
			missing, err := missingCommits(ctx, info.MirrorPath, commits)
			if err != nil || len(missing) == 0 {
				return nil, "", err
			}
			if depth <= 0 {
				return nil, "", fmt.Errorf("%w: %s needs commits of itself not on its branches", ErrSubmoduleDepth, info.RepoName)
			}
			translated, gone, err := m.translateCommits(ctx, info, missing, progressWriter, depth-1)
			if err != nil {
				return nil, "", submoduleError(submodule, info.RepoName, err)
			}
			maps.Copy(t.translated, translated)
			maps.Copy(res.decided, translated)
			return gone, "commit gone upstream", nil
		}
		if depth <= 0 {
			return nil, "", fmt.Errorf("%w: %s needs submodule %s", ErrSubmoduleDepth, info.RepoName, submodule)
		}

		var gone []string
		if beingConverted(ctx, canonicalRepoID(subInfo)) {
			// Its conversion waits for this one, so only the commits recorded here are translated
			var translated map[sha1ID]sha256ID
			translated, gone, err = m.translateCommits(ctx, subInfo, commits, progressWriter, depth-1)
			maps.Copy(t.translated, translated)
			maps.Copy(res.decided, translated)
		} else {
			var table *translationIndex
			table, gone, err = m.ensureSubmoduleCommits(ctx, subInfo, commits, progressWriter, depth-1)
			if table != nil {
				t.tables = append(t.tables, table)
				res.tables = append(res.tables, table)
			}
		}
		if errors.Is(err, ErrRedirectNotAllowed) {
			// The administrator can allow the host redirected to, or approve synthetic SHA-256s
			return commits, err.Error(), nil
		}
		if err != nil {
			return nil, "", submoduleError(submodule, info.RepoName, err)
		}
		return gone, "commit or repository gone upstream", nil
	}

	// Commits that cannot be converted from the submodule recording them, decided once all
	// submodules are converted
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
	if progressWriter != nil && len(submodules) > 0 {
		_, _ = fmt.Fprintf(progressWriter, "remote: Evergit: [%s] Resolving %d submodule(s)...\n", info.RepoName, len(submodules))
	}
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
		res.recorded[submodule] = pending
		failed, reason, err := convertSubmodule(submodule, pending)
		if err != nil {
			return nil, err
		}
		if len(failed) > 0 {
			problems = append(problems, problem{submodule: submodule, reason: reason, commits: failed})
		}
	}

	// The same commit may be recorded under other submodules elsewhere in the history, and a
	// submodule that moved, e.g. from git.qemu.org to gitlab.com, has the commits recorded under its
	// old URLs: such a commit is translated from those, as a commit is the same in any repository
	// containing it. The decision depends only on the history, never on what other repositories had
	// converted before, so it is the same in any order and however far the repository is converted
	recording := targets
	var aliases map[string][]string
	if len(problems) > 0 {
		if heads != nil {
			if recording, err = discoverGitlinks(ctx, info.MirrorPath, nil, exclude, nil); err != nil {
				return nil, err
			}
		}
		if aliases, err = submoduleAliases(ctx, info.MirrorPath); err != nil {
			return nil, err
		}
	}
	recorders := make([]string, 0, len(recording))
	for submodule := range recording {
		recorders = append(recorders, submodule)
	}
	slices.Sort(recorders)
	// candidates lists the other submodules a commit can be translated from, in order of precedence
	candidates := func(submodule, commit string) []string {
		var list []string
		for _, other := range recorders {
			if other != submodule && recording[other][commit] {
				list = append(list, other)
			}
		}
		for _, other := range aliases[submodule] {
			if !slices.Contains(list, other) {
				list = append(list, other)
			}
		}
		return list
	}
	// The synthetic SHA-256 is only recorded if the administrator approved it
	var unapproved, unapprovedReasons []string
	for _, p := range problems {
		var remaining []string
		for _, c := range p.commits {
			id, err := parseSHA1Hex(c)
			if err != nil {
				return nil, err
			}
			resolved := false
			for _, other := range candidates(p.submodule, c) {
				if _, _, ok := lookup(other, id); !ok {
					if _, _, err := convertSubmodule(other, []string{c}); err != nil {
						return nil, err
					}
				}
				if newID, own, ok := lookup(other, id); ok {
					if !own {
						res.decided[id] = newID
					}
					resolved = true
					break
				}
			}
			if !resolved {
				remaining = append(remaining, c)
			}
		}
		if len(remaining) == 0 {
			continue
		}
		approval, ok := config.FindSyntheticApproval(approvals, superproject, p.submodule)
		if !ok {
			unapproved = append(unapproved, p.submodule)
			unapprovedReasons = append(unapprovedReasons, fmt.Sprintf("  %s: %s (%d commit(s))\n", p.submodule, p.reason, len(remaining)))
			continue
		}
		log.Printf("[%s] Recording %d commit(s) of submodule %q with the synthetic SHA-256 as approved by %q: %s", info.RepoName, len(remaining), p.submodule, approval, p.reason)
		for _, c := range remaining {
			id, _ := parseSHA1Hex(c)
			res.decided[id] = syntheticID(id)
		}
		res.synthetic[p.submodule] = syntheticDecision{Reason: p.reason, ApprovedBy: approval.String()}
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

// submoduleError reports a failure to convert a submodule, as temporary unless it is known why.
func submoduleError(submodule, repoName string, err error) error {
	if errors.Is(err, ErrSubmoduleCycle) || errors.Is(err, ErrSubmoduleDepth) || errors.Is(err, ErrSubmoduleUnavailable) || errors.Is(err, ErrSubmoduleNotApproved) {
		return fmt.Errorf("submodule %s of %s: %w", submodule, repoName, err)
	}
	return fmt.Errorf("%w: submodule %s of %s: %w", ErrSubmoduleUnavailable, submodule, repoName, err)
}

// syntheticGitlinksFile is the administrator's list of submodules approved for synthetic SHA-256s.
func (m *Manager) syntheticGitlinksFile() string {
	if m.cfg == nil {
		return ""
	}
	return m.cfg.SyntheticGitlinksFile
}

// ensureSubmoduleCommits converts a submodule so that its translation table covers commits, and
// returns the table and the commits that do not exist upstream. A submodule not converted completely
// before is converted only as far as the history of the commits superprojects record (see
// partialConversion). Commits it translates already need no fetch: commits never change.
func (m *Manager) ensureSubmoduleCommits(ctx context.Context, subInfo *resolver.RepositoryInfo, commits []string, progressWriter io.Writer, depth int) (*translationIndex, []string, error) {
	// missingFrom opens the submodule's index (none for a natively SHA-256 submodule, whose
	// commits have no SHA-1, or for one converted with other rules, which is rebuilt) and lists the
	// commits it lacks
	missingFrom := func() (*translationIndex, []string) {
		var table *translationIndex
		if currentState(subInfo.ServingPath) != nil && ensureTranslationIndexes(subInfo.ServingPath) == nil {
			table, _ = openTranslationIndex(filepath.Join(subInfo.ServingPath, sha1IndexFile), len(sha1ID{}))
		}
		var missing []string
		for _, c := range commits {
			id, err := parseSHA1Hex(c)
			if _, ok := table.lookup(id[:]); err != nil || !ok {
				missing = append(missing, c)
			}
		}
		return table, missing
	}

	table, missing := missingFrom()
	if len(missing) == 0 {
		return table, nil, nil
	}

	// Fetched regardless of the TTL, including commits that no branch or tag reaches
	_ = table.Close()
	err := m.ensureRepo(ctx, subInfo, progressWriter, depth, missing, true)
	if err != nil && !errors.Is(err, errUpstreamGone) {
		return nil, nil, err
	}
	table, missing = missingFrom()
	return table, missing, nil
}

// translateCommits computes the SHA-256 of submodule commits of a repository being converted: further
// up the chain of submodules, whose conversion waits for this one, or this one itself. A commit can
// only record commits that existed before it, so recording each other's commits never leads two
// repositories back to the same commit: the history of the commits is translated in memory, writing
// nothing, as the repository's own conversion translates it. It returns the translations and the
// commits that do not exist upstream.
func (m *Manager) translateCommits(ctx context.Context, subInfo *resolver.RepositoryInfo, commits []string, progressWriter io.Writer, depth int) (map[sha1ID]sha256ID, []string, error) {
	log.Printf("[%s] Translating %d submodule commit(s) of a repository being converted", subInfo.RepoName, len(commits))
	// The conversion synced the mirror; missing commits are fetched without changing its refs
	missing, err := missingCommits(ctx, subInfo.MirrorPath, commits)
	if err != nil {
		return nil, nil, err
	}
	var gone []string
	for _, c := range missing {
		err := m.gitUpstream(ctx, subInfo, subInfo.MirrorPath, func(remote string) []string {
			return []string{"-c", "http.followRedirects=false", "fetch", "--no-tags", "--no-write-fetch-head", remote, c}
		})
		if err := markGone(err); errors.Is(err, errUpstreamGone) {
			gone = append(gone, c)
		} else if err != nil {
			return nil, nil, fmt.Errorf("failed to fetch submodule commit %s: %w", c, err)
		}
	}
	heads := slices.DeleteFunc(slices.Clone(commits), func(c string) bool { return slices.Contains(gone, c) })
	if len(heads) == 0 {
		return nil, gone, nil
	}

	gitlinks, err := m.resolveGitlinks(ctx, subInfo, progressWriter, depth, heads)
	if err != nil {
		return nil, nil, err
	}
	defer gitlinks.close()
	// History converted before is looked up in the translation table, as in an incremental conversion
	ids := &translations{added: make(map[sha1ID]sha256ID)}
	var exclude []string
	if state := currentState(subInfo.ServingPath); state != nil && isIncrementalRewrite(subInfo.ServingPath) {
		if err := ensureTranslationIndexes(subInfo.ServingPath); err != nil {
			return nil, nil, fmt.Errorf("failed to index loose-object-idx: %w", err)
		}
		if ids.known, err = openTranslationIndex(filepath.Join(subInfo.ServingPath, sha1IndexFile), len(sha1ID{})); err != nil {
			return nil, nil, err
		}
		defer ids.known.Close()
		for _, id := range state.ConvertedRefs {
			exclude = append(exclude, id)
		}
	}
	if _, _, err := rewriteObjects(ctx, subInfo.MirrorPath, "", ids, gitlinks, heads, exclude, nil); err != nil {
		return nil, nil, err
	}

	translated := make(map[sha1ID]sha256ID, len(heads))
	for _, c := range heads {
		id, err := parseSHA1Hex(c)
		if err != nil {
			return nil, nil, err
		}
		newID, ok := ids.lookup(id)
		if !ok {
			return nil, nil, fmt.Errorf("submodule commit %s of %s was not translated", c, subInfo.RepoName)
		}
		translated[id] = newID
	}
	return translated, gone, nil
}

// pinCommits makes required commits part of the mirror's history under refs/evergit-pinned/, fetching
// them by id when the mirror lacks them. Commits the upstream does not have are skipped; other
// failures are returned.
func (m *Manager) pinCommits(ctx context.Context, info *resolver.RepositoryInfo, requiredCommits []string) error {
	for _, c := range requiredCommits {
		if _, err := parseSHA1Hex(c); err != nil {
			return err
		}
		ref := pinnedPrefix + c
		if _, err := runCmdContext(ctx, 0, info.MirrorPath, "git", "cat-file", "-e", c+"^{commit}"); err == nil {
			if _, err := runCmdContext(ctx, 0, info.MirrorPath, "git", "update-ref", ref, c); err != nil {
				return err
			}
		} else if err := m.gitUpstream(ctx, info, info.MirrorPath, func(remote string) []string {
			return []string{"-c", "http.followRedirects=false", "fetch", "--no-tags", remote, "+" + c + ":" + ref}
		}); err != nil {
			if err := markGone(err); errors.Is(err, errUpstreamGone) {
				log.Printf("[%s] Submodule commit %s does not exist upstream: %v", info.RepoName, c, err)
				continue
			}
			return fmt.Errorf("failed to fetch submodule commit %s: %w", c, err)
		}
	}
	return nil
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
