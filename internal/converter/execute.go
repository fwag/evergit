package converter

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// A plan is executed in dependency order: before a repository is converted, the repositories its
// gitlinks come from translate the commits it records. A gitlink only records a commit that existed
// before the commit recording it, so following the gitlinks of a history always goes back in time,
// even across repositories recording each other: converting one commit of a cycle may require older
// commits of the other repositories of the cycle, never itself.

// convert makes an item translate commits, or all its refs with nil, converting the repositories its
// gitlinks come from first.
func (p *conversionPlan) convert(r *planItem, commits []string) error {
	complete := commits == nil || r.rebuild
	var heads []string
	if complete {
		if p.completed[r] {
			return nil
		}
	} else {
		var err error
		if heads, err = p.untranslated(r, commits); err != nil || len(heads) == 0 {
			return err
		}
	}
	key := r.id + " " + strings.Join(heads, " ")
	if p.converting[key] {
		return fmt.Errorf("%w: converting %s requires converting itself", ErrSubmoduleUnavailable, key)
	}
	p.converting[key] = true
	defer delete(p.converting, key)

	needs, err := p.needs(r, heads, complete)
	if err != nil {
		return err
	}
	for _, id := range sortedKeys(needs) {
		if err := p.convert(p.items[id], needs[id]); err != nil {
			return err
		}
	}
	if err := p.step(r, heads, complete); err != nil {
		return err
	}
	if complete {
		p.completed[r] = true
	}
	return nil
}

// plannedGitlink is a gitlink of an item and the source the plan decided for it.
type plannedGitlink struct {
	submodule, commit string
	id                sha1ID
	src               gitlinkSource
}

// planned lists an item's gitlinks with a planned source, in a stable order, without those
// recorded by earlier conversions.
func (r *planItem) planned() ([]plannedGitlink, error) {
	var links []plannedGitlink
	for _, submodule := range sortedKeys(r.sources) {
		for _, c := range sortedKeys(r.sources[submodule]) {
			id, err := parseSHA1Hex(c)
			if err != nil {
				return nil, err
			}
			if _, ok := r.decided[id]; !ok {
				links = append(links, plannedGitlink{submodule, c, id, r.sources[submodule][c]})
			}
		}
	}
	return links, nil
}

// stepGitlinks lists the planned gitlinks of the history of heads of an item.
func (p *conversionPlan) stepGitlinks(r *planItem, heads []string) ([]plannedGitlink, error) {
	targets, err := discoverGitlinks(p.ctx, r.info.MirrorPath, heads, convertedRefs(r), nil)
	if err != nil {
		return nil, err
	}
	all, err := r.planned()
	if err != nil {
		return nil, err
	}
	var links []plannedGitlink
	for _, l := range all {
		if targets[l.submodule][l.commit] {
			links = append(links, l)
			delete(targets[l.submodule], l.commit)
		}
	}
	for submodule, commits := range targets {
		for c := range commits {
			id, err := parseSHA1Hex(c)
			if _, recorded := r.decided[id]; err == nil && !recorded {
				return nil, fmt.Errorf("gitlink %s of submodule %s in %s was not planned", c, submodule, r.id)
			}
		}
	}
	return links, nil
}

// needs returns, by repository, the commits that must be translated before converting the history
// of heads of an item, or all of its planned gitlinks for a complete conversion.
func (p *conversionPlan) needs(r *planItem, heads []string, complete bool) (map[string][]string, error) {
	var links []plannedGitlink
	var err error
	if complete {
		links, err = r.planned()
	} else {
		links, err = p.stepGitlinks(r, heads)
	}
	if err != nil {
		return nil, err
	}
	needs := make(map[string][]string)
	for _, l := range links {
		// A complete conversion converts the commits of the repository itself it records
		repo := l.src.repo
		if repo != nil && (repo != r || !complete) && !slices.Contains(needs[repo.id], l.commit) {
			needs[repo.id] = append(needs[repo.id], l.commit)
		}
	}
	return needs, nil
}

// convertedRefs returns the refs a repository was converted for, whose history is converted.
func convertedRefs(r *planItem) []string {
	var refs []string
	if state := currentState(r.info.ServingPath); state != nil && isIncrementalRewrite(r.info.ServingPath) {
		for _, id := range state.ConvertedRefs {
			refs = append(refs, id)
		}
	}
	return refs
}

// step converts an item: all refs, or only the history of heads, as a partial conversion (pinning
// heads) or by adding their objects to a repository converted completely.
func (p *conversionPlan) step(r *planItem, heads []string, complete bool) error {
	mu := p.m.repoMutex(r.info)
	mu.Lock()
	defer mu.Unlock()

	res, err := p.resolution(r)
	if err != nil {
		return err
	}
	var scope conversionScope
	switch {
	case complete:
	case partialConversion(r.info.ServingPath):
		if _, err := p.m.fetchCommits(p.ctx, r.info, heads, true); err != nil {
			return fmt.Errorf("%s: %w", r.id, err)
		}
		scope.partial, scope.pins = true, heads
	default:
		scope.objects = heads
	}
	if err := p.m.convertRepo(p.ctx, r.info, p.progressWriter, res, scope); err != nil {
		return fmt.Errorf("%s: %w", r.id, err)
	}
	return nil
}

// resolution decides an item's gitlinks: the decisions recorded before, and those of the plan whose
// source translates its commit by now.
func (p *conversionPlan) resolution(r *planItem) (*gitlinkResolution, error) {
	var decided map[sha1ID]sha256ID
	if previous, err := loadState(r.info.ServingPath); err == nil {
		if decided, err = previous.gitlinkDecisions(); err != nil {
			return nil, err
		}
	}
	res := newGitlinkResolution(decided)
	links, err := r.planned()
	if err != nil {
		return nil, err
	}
	tables := make(map[*planItem]*translationIndex)
	defer func() {
		for _, table := range tables {
			_ = table.Close()
		}
	}()
	for _, l := range links {
		// Without a value: one of the repository's own commits, or not needed by this step
		if newID, ok := p.value(l.src, l.id, tables); ok {
			if err := res.add(l, newID); err != nil {
				return nil, err
			}
		}
	}
	return res, nil
}

// add records the SHA-256 a planned gitlink gets, unless already decided, e.g. by an earlier step of
// the same plan.
func (g *gitlinkResolution) add(l plannedGitlink, newID sha256ID) error {
	if previous, set := g.decided[l.id]; set {
		if previous != newID {
			return fmt.Errorf("submodule commit %s has conflicting SHA-256 translations %x and %x", l.commit, previous, newID)
		}
		return nil
	}
	g.decided[l.id] = newID
	g.recorded[l.submodule] = append(g.recorded[l.submodule], l.commit)
	if l.src.synthetic != nil {
		g.synthetic[l.submodule] = *l.src.synthetic
	}
	return nil
}

// value returns the SHA-256 a source gives a commit, if it is known: synthetic, or translated by a
// repository converted by now.
func (p *conversionPlan) value(src gitlinkSource, id sha1ID, tables map[*planItem]*translationIndex) (sha256ID, bool) {
	if src.synthetic != nil {
		return syntheticID(id), true
	}
	if src.repo == nil {
		return sha256ID{}, false
	}
	table, opened := tables[src.repo]
	if !opened {
		if currentState(src.repo.info.ServingPath) != nil && ensureTranslationIndexes(src.repo.info.ServingPath) == nil {
			table, _ = openTranslationIndex(filepath.Join(src.repo.info.ServingPath, sha1IndexFile), len(sha1ID{}))
		}
		tables[src.repo] = table
	}
	value, ok := table.lookup(id[:])
	var newID sha256ID
	copy(newID[:], value)
	return newID, ok
}
