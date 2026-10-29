package converter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"evergit/internal/config"
	"evergit/internal/resolver"
)

// A conversion is planned before anything is converted. Starting from the requested repository, the
// plan walks the gitlinks breadth-first, makes the submodule commits they record available in the
// submodules' mirrors, and decides where the SHA-256 of every gitlink comes from. Missing approvals
// are thus all known before converting, and the plan is then executed in dependency order.

// gitlinkSource is where the SHA-256 of a submodule commit comes from: a repository translating it
// (possibly the superproject itself, for a commit of its own no branch reaches), one of the
// superproject's commits converted with it, or an approved synthetic SHA-256.
type gitlinkSource struct {
	repo      *planItem
	own       bool
	synthetic *syntheticDecision
}

// planItem is a repository of a plan, and the commits it must translate.
type planItem struct {
	info *resolver.RepositoryInfo
	id   string
	// complete: all refs are converted (the requested repository, or one rebuilt), otherwise the
	// required commits
	complete bool
	// rebuild: converted completely with other rules, so converted completely again rather than
	// extended
	rebuild  bool
	required []string
	scanned  int  // required commits already scanned
	queued   bool // waiting for a scan
	synced   bool

	unavailable error           // the repository cannot be fetched (gone, redirect not allowed)
	gone        map[string]bool // required commits the upstream does not have
	present     map[string]bool // commits checked to be in the mirror before any fetch

	decided  map[sha1ID]sha256ID // decisions recorded by earlier conversions
	exclude  []string            // refs converted before, whose gitlinks are decided
	heads    []string            // commits whose history was scanned
	gitlinks gitlinkTargets      // gitlinks of the history to convert, by submodule
	sources  map[string]map[string]gitlinkSource

	recording gitlinkTargets // gitlinks of the whole history, for problems
	aliases   map[string][]string
}

// conversionPlan decides the gitlinks of a conversion and of the submodules it needs.
type conversionPlan struct {
	m              *Manager
	ctx            context.Context
	progressWriter io.Writer
	maxRepos       int // submodule repositories
	approvals      []config.SyntheticGitlink

	top   *planItem
	items map[string]*planItem
	queue []*planItem
	// execution: items converted completely, and the conversions in progress
	completed  map[*planItem]bool
	converting map[string]bool
	// missing approvals, by superproject
	unapproved map[string][]unapprovedGitlinks
}

type unapprovedGitlinks struct {
	submodule, reason string
	commits           int
}

// planConversion plans the conversion of info: of all its refs, or with commits, only of their
// history (as for a submodule). The repository's mirror must be synced.
func (m *Manager) planConversion(ctx context.Context, info *resolver.RepositoryInfo, commits []string, progressWriter io.Writer, maxRepos int) (*conversionPlan, error) {
	approvals, err := config.LoadSyntheticGitlinks(m.syntheticGitlinksFile())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	p := &conversionPlan{
		m: m, ctx: ctx, progressWriter: progressWriter, maxRepos: maxRepos, approvals: approvals,
		items: make(map[string]*planItem), unapproved: make(map[string][]unapprovedGitlinks),
		completed: make(map[*planItem]bool), converting: make(map[string]bool),
	}
	p.top, err = p.item(info)
	if err != nil {
		return nil, err
	}
	p.top.synced = true
	p.top.complete = p.top.complete || commits == nil
	p.require(p.top, commits...)
	if err := p.walk(); err != nil {
		return nil, err
	}
	return p, p.approvalError()
}

// item returns the plan's item for a repository, adding it.
func (p *conversionPlan) item(info *resolver.RepositoryInfo) (*planItem, error) {
	id := canonicalRepoID(info)
	if r, ok := p.items[id]; ok {
		return r, nil
	}
	if p.top != nil && len(p.items)-1 >= p.maxRepos {
		return nil, fmt.Errorf("%w: %s needs more than %d (-max-submodule-repos), the next being %s", ErrTooManySubmodules, p.top.id, p.maxRepos, id)
	}
	r := &planItem{info: info, id: id, gone: make(map[string]bool), present: make(map[string]bool),
		gitlinks: make(gitlinkTargets), sources: make(map[string]map[string]gitlinkSource)}
	if previous, err := loadState(info.ServingPath); err == nil {
		if r.decided, err = previous.gitlinkDecisions(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	r.exclude = convertedRefs(r)
	p.items[id] = r
	if previous, err := loadState(info.ServingPath); err == nil && !isIncrementalRewrite(info.ServingPath) {
		if !previous.Partial {
			r.rebuild, r.complete = true, true
		} else {
			// Rebuilt with all the commits superprojects required before
			p.require(r, slices.Sorted(maps.Values(pinnedRefs(previous.ConvertedRefs)))...)
		}
	}
	return r, nil
}

// require adds commits an item must translate, queueing it for a scan of their history.
func (p *conversionPlan) require(r *planItem, commits ...string) {
	for _, c := range commits {
		if !slices.Contains(r.required, c) {
			r.required = append(r.required, c)
		}
	}
	if !r.queued && (r.scanned < len(r.required) || (r.complete && r.heads == nil)) {
		r.queued = true
		p.queue = append(p.queue, r)
	}
}

// walk scans the queued items until every gitlink of the plan has a source.
func (p *conversionPlan) walk() error {
	for {
		for len(p.queue) > 0 {
			r := p.queue[0]
			p.queue = p.queue[1:]
			r.queued = false
			if err := p.scan(r); err != nil {
				return err
			}
		}
		progressed := false
		for _, id := range p.sortedIDs() {
			done, err := p.decide(p.items[id])
			if err != nil {
				return err
			}
			progressed = progressed || done
		}
		if !progressed && len(p.queue) == 0 {
			return nil
		}
	}
}

func (p *conversionPlan) sortedIDs() []string {
	ids := make([]string, 0, len(p.items))
	for id := range p.items {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// scan makes an item's new required commits available and lists the gitlinks of their history.
func (p *conversionPlan) scan(r *planItem) error {
	heads, ok, err := p.prepare(r)
	if err != nil || !ok {
		return err
	}
	targets, err := p.discover(r, heads)
	if err != nil {
		return err
	}
	if heads == nil {
		r.heads = []string{} // scanned completely
	} else {
		r.heads = append(r.heads, heads...)
	}
	for submodule, commits := range targets {
		if r.gitlinks[submodule] == nil {
			r.gitlinks[submodule] = make(map[string]bool)
		}
		for c := range commits {
			r.gitlinks[submodule][c] = true
		}
	}
	return nil
}

// discover lists the gitlinks of the history of heads of an item not scanned before, reporting
// progress to the client.
func (p *conversionPlan) discover(r *planItem, heads []string) (gitlinkTargets, error) {
	if p.progressWriter == nil {
		return discoverGitlinks(p.ctx, r.info.MirrorPath, heads, append(slices.Clone(r.exclude), r.heads...), nil)
	}
	_, _ = fmt.Fprintf(p.progressWriter, "remote: Evergit: [%s] Scanning history for submodules...\n", r.info.RepoName)
	scanned := 0
	targets, err := discoverGitlinks(p.ctx, r.info.MirrorPath, heads, append(slices.Clone(r.exclude), r.heads...), func(n int) {
		scanned = n
		_, _ = fmt.Fprintf(p.progressWriter, "\rremote: Evergit: [Progress] Scanning %s: %d commits...", r.info.RepoName, n)
	})
	if err == nil && scanned > 0 {
		_, _ = fmt.Fprintf(p.progressWriter, "\rremote: Evergit: [Progress] Scanning %s: %d commits, done.\n", r.info.RepoName, scanned)
	}
	return targets, err
}

// prepare fetches an item's new required commits that its serving repository does not translate,
// and returns the commits whose history to scan (nil: all refs), or false if there are none.
func (p *conversionPlan) prepare(r *planItem) ([]string, bool, error) {
	firstComplete := r.complete && r.heads == nil
	commits := r.required[r.scanned:]
	r.scanned = len(r.required)
	if r.unavailable != nil {
		return nil, false, nil
	}
	todo, err := p.untranslated(r, commits)
	if err != nil {
		return nil, false, err
	}
	if len(todo) > 0 {
		if err := p.fetch(r, todo); err != nil || r.unavailable != nil {
			return nil, false, err
		}
	}
	if firstComplete {
		return nil, true, nil
	}
	heads := slices.DeleteFunc(todo, func(c string) bool { return r.gone[c] })
	return heads, len(heads) > 0, nil
}

// untranslated returns the commits an item's serving repository does not translate yet. Commits of
// a repository converted with other rules are all untranslated, as it is rebuilt.
func (p *conversionPlan) untranslated(r *planItem, commits []string) ([]string, error) {
	var table *translationIndex
	if currentState(r.info.ServingPath) != nil && ensureTranslationIndexes(r.info.ServingPath) == nil {
		table, _ = openTranslationIndex(filepath.Join(r.info.ServingPath, sha1IndexFile), len(sha1ID{}))
		defer table.Close()
	}
	var todo []string
	for _, c := range commits {
		id, err := parseSHA1Hex(c)
		if err != nil {
			return nil, err
		}
		if _, ok := table.lookup(id[:]); !ok {
			todo = append(todo, c)
		}
	}
	return todo, nil
}

// fetch syncs a submodule's mirror, once per plan, and fetches the commits it lacks without pinning
// them: an execution step pins the commits it converts.
func (p *conversionPlan) fetch(r *planItem, commits []string) error {
	mu := p.m.repoMutex(r.info)
	mu.Lock()
	defer mu.Unlock()
	if !r.synced {
		r.synced = true
		err := p.m.syncMirror(p.ctx, r.info)
		switch {
		case errors.Is(err, errUpstreamGone) || errors.Is(err, ErrRedirectNotAllowed):
			r.unavailable = err
			return nil
		case err != nil:
			return fmt.Errorf("%w: %s: %w", ErrSubmoduleUnavailable, r.id, err)
		}
		if isRepoSHA256(r.info.MirrorPath) {
			// Natively SHA-256: the SHA-1 commits superprojects record do not exist in it
			r.unavailable = fmt.Errorf("%w: %s is natively SHA-256", errUpstreamGone, r.id)
			return nil
		}
	}
	missing, err := missingCommits(p.ctx, r.info.MirrorPath, commits)
	if err != nil {
		return err
	}
	for _, c := range commits {
		r.present[c] = !slices.Contains(missing, c)
	}
	gone, err := p.m.fetchCommits(p.ctx, r.info, missing, false)
	if errors.Is(err, ErrRedirectNotAllowed) {
		r.unavailable = err
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrSubmoduleUnavailable, r.id, err)
	}
	for _, c := range gone {
		r.gone[c] = true
	}
	return nil
}

// recordedGitlink is a submodule commit recorded in a repository's history.
type recordedGitlink struct{ submodule, commit, reason string }

// decide gives a source to every gitlink of an item without one, and reports whether it decided any.
func (p *conversionPlan) decide(r *planItem) (bool, error) {
	links, err := r.undecided()
	if err != nil {
		return false, err
	}
	p.reportResolving(r, links)
	var problems []recordedGitlink
	decided := false
	for _, l := range links {
		src, reason, err := p.sourceOf(r, l.submodule, l.commit)
		switch {
		case err != nil:
			return false, err
		case src == nil:
			// its repository is being scanned
		case reason != "":
			l.reason = reason
			problems = append(problems, l)
		default:
			p.setSource(r, l.submodule, l.commit, *src)
			decided = true
		}
	}
	solved, err := p.decideProblems(r, problems)
	return decided || solved, err
}

// reportResolving tells the client how many submodules of an item are being decided.
func (p *conversionPlan) reportResolving(r *planItem, links []recordedGitlink) {
	if p.progressWriter == nil || len(links) == 0 {
		return
	}
	submodules := make(map[string]bool)
	for _, l := range links {
		submodules[l.submodule] = true
	}
	_, _ = fmt.Fprintf(p.progressWriter, "remote: Evergit: [%s] Resolving %d submodule(s)...\n", r.info.RepoName, len(submodules))
}

// undecided lists the gitlinks of an item that neither an earlier conversion nor the plan decided.
func (r *planItem) undecided() ([]recordedGitlink, error) {
	var links []recordedGitlink
	for _, submodule := range sortedKeys(r.gitlinks) {
		for _, c := range sortedKeys(r.gitlinks[submodule]) {
			id, err := parseSHA1Hex(c)
			if err != nil {
				return nil, err
			}
			_, recorded := r.decided[id]
			_, planned := r.sources[submodule][c]
			if !recorded && !planned {
				links = append(links, recordedGitlink{submodule: submodule, commit: c})
			}
		}
	}
	return links, nil
}

// decideProblems decides the gitlinks that cannot come from the submodule recording them, from
// candidates or else with the synthetic SHA-256, and reports whether it decided any.
func (p *conversionPlan) decideProblems(r *planItem, problems []recordedGitlink) (bool, error) {
	unsolved := make(map[[2]string][]string)
	var order [][2]string
	decided := false
	for _, pr := range problems {
		src, err := p.candidateSource(r, pr.submodule, pr.commit)
		if err != nil {
			return false, err
		}
		if src == nil {
			continue // a candidate is being scanned
		}
		decided = true
		if *src != (gitlinkSource{}) {
			p.setSource(r, pr.submodule, pr.commit, *src)
			continue
		}
		key := [2]string{pr.submodule, pr.reason}
		if _, seen := unsolved[key]; !seen {
			order = append(order, key)
		}
		unsolved[key] = append(unsolved[key], pr.commit)
	}
	for _, key := range order {
		p.synthetic(r, key[0], key[1], unsolved[key])
	}
	return decided, nil
}
func (p *conversionPlan) setSource(r *planItem, submodule, commit string, src gitlinkSource) {
	if r.sources[submodule] == nil {
		r.sources[submodule] = make(map[string]gitlinkSource)
	}
	r.sources[submodule][commit] = src
}

// sourceOf returns the source of a gitlink of an item from the submodule recording it, a reason it
// cannot come from there, or nil while the submodule's repository is being scanned.
func (p *conversionPlan) sourceOf(r *planItem, submodule, commit string) (*gitlinkSource, string, error) {
	if strings.HasPrefix(submodule, pathPrefix) {
		return &gitlinkSource{}, "no URL in the recording commit's .gitmodules", nil
	}
	subInfo, err := p.m.resolveSubmoduleInfo(submodule, r.info)
	if err != nil {
		return &gitlinkSource{}, "URL cannot be mirrored: " + err.Error(), nil
	}
	if canonicalRepoID(subInfo) == r.id {
		return p.ownSource(r, commit)
	}
	sub, err := p.item(subInfo)
	if err != nil {
		return nil, "", err
	}
	if !slices.Contains(sub.required, commit) {
		p.require(sub, commit)
		return nil, "", nil
	}
	if sub.queued {
		return nil, "", nil
	}
	if errors.Is(sub.unavailable, ErrRedirectNotAllowed) {
		// The administrator can allow the host redirected to, or approve synthetic SHA-256s
		return &gitlinkSource{}, sub.unavailable.Error(), nil
	}
	if sub.unavailable != nil || sub.gone[commit] {
		return &gitlinkSource{}, "commit or repository gone upstream", nil
	}
	return &gitlinkSource{repo: sub}, "", nil
}

// ownSource returns the source of a commit of the repository itself: converted with it if its mirror
// has it, otherwise fetched and translated by the repository as a commit it must translate.
func (p *conversionPlan) ownSource(r *planItem, commit string) (*gitlinkSource, string, error) {
	if _, checked := r.present[commit]; !checked {
		missing, err := missingCommits(p.ctx, r.info.MirrorPath, []string{commit})
		if err != nil {
			return nil, "", err
		}
		r.present[commit] = len(missing) == 0
	}
	if r.present[commit] {
		return &gitlinkSource{own: true}, "", nil
	}
	if !slices.Contains(r.required, commit) {
		p.require(r, commit)
		return nil, "", nil
	}
	if r.queued {
		return nil, "", nil
	}
	if r.gone[commit] {
		return &gitlinkSource{}, "commit gone upstream", nil
	}
	return &gitlinkSource{repo: r}, "", nil
}

// candidateSource translates a commit that cannot be converted from the submodule recording it
// from the other submodules recording the same commit in the history, then from the other URLs the
// submodule's path had: a commit is the same in any repository containing it. The decision depends
// only on the history, never on what other repositories had converted before, so it is the same in
// any order and however far the repository is converted.
// It returns nil while a candidate is being scanned, and a zero source if no candidate has the commit.
func (p *conversionPlan) candidateSource(r *planItem, submodule, commit string) (*gitlinkSource, error) {
	candidates, err := p.candidates(r, submodule, commit)
	if err != nil {
		return nil, err
	}
	for _, other := range candidates {
		src, reason, err := p.sourceOf(r, other, commit)
		if err != nil || src == nil {
			return nil, err
		}
		if reason == "" {
			return src, nil
		}
	}
	return &gitlinkSource{}, nil
}

// candidates lists the other submodules a commit can be translated from, in order of precedence.
func (p *conversionPlan) candidates(r *planItem, submodule, commit string) ([]string, error) {
	if r.recording == nil {
		r.recording = r.gitlinks
		if !r.complete {
			var err error
			if r.recording, err = discoverGitlinks(p.ctx, r.info.MirrorPath, nil, r.exclude, nil); err != nil {
				return nil, err
			}
		}
		var err error
		if r.aliases, err = submoduleAliases(p.ctx, r.info.MirrorPath); err != nil {
			return nil, err
		}
	}
	var list []string
	for _, other := range sortedKeys(r.recording) {
		if other != submodule && r.recording[other][commit] {
			list = append(list, other)
		}
	}
	for _, other := range r.aliases[submodule] {
		if !slices.Contains(list, other) {
			list = append(list, other)
		}
	}
	return list, nil
}

// synthetic records commits with the synthetic SHA-256 if the administrator approved it; otherwise
// they keep a zero source and are reported (approvalError).
func (p *conversionPlan) synthetic(r *planItem, submodule, reason string, commits []string) {
	approval, ok := config.FindSyntheticApproval(p.approvals, r.id, submodule)
	if !ok {
		p.unapproved[r.id] = append(p.unapproved[r.id], unapprovedGitlinks{submodule, reason, len(commits)})
		for _, c := range commits {
			p.setSource(r, submodule, c, gitlinkSource{})
		}
		return
	}
	log.Printf("[%s] Recording %d commit(s) of submodule %q with the synthetic SHA-256 as approved by %q: %s", r.info.RepoName, len(commits), submodule, approval, reason)
	decision := &syntheticDecision{Reason: reason, ApprovedBy: approval.String()}
	for _, c := range commits {
		p.setSource(r, submodule, c, gitlinkSource{synthetic: decision})
	}
}

// approvalError reports every missing approval of the plan at once.
func (p *conversionPlan) approvalError() error {
	if len(p.unapproved) == 0 {
		return nil
	}
	var reasons, hint strings.Builder
	for _, superproject := range sortedKeys(p.unapproved) {
		fmt.Fprintf(&reasons, "submodules of %s cannot be converted:\n", superproject)
		for _, u := range p.unapproved[superproject] {
			fmt.Fprintf(&reasons, "  %s: %s (%d commit(s))\n", u.submodule, u.reason, u.commits)
			fmt.Fprintf(&hint, "  %s %s\n", superproject, u.submodule)
		}
	}
	return fmt.Errorf("%w: %s\nAsk the Evergit administrator to allow their hosts (-allowed-hosts), or to approve synthetic SHA-256s for them in the synthetic gitlinks file (-synthetic-gitlinks):\n%s",
		ErrSubmoduleNotApproved, reasons.String(), hint.String())
}

// summary describes the plan for clients' progress.
func (p *conversionPlan) summary() string {
	toConvert := 0
	for _, r := range p.items {
		if r.heads != nil {
			toConvert++
		}
	}
	return fmt.Sprintf("%d repositories, %d to convert", len(p.items), toConvert)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
