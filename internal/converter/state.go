package converter

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
)

// conversionVersion identifies the rules that translate SHA-1 objects to SHA-256 objects. Bump it
// whenever a change could alter any object id: serving repositories recorded with another version
// are rebuilt, never extended. Object ids do not depend on the installed git version (see
// rewrite.go), so git upgrades do not require a bump.
const conversionVersion = 1

// stateFile records how a serving repository was converted, in its git directory (never served).
const stateFile = "evergit-state.json"

// repoState is the content of stateFile.
type repoState struct {
	ConversionVersion int `json:"conversion_version"`
	// EvergitRevision is the build that last converted the repository, for diagnostics only
	EvergitRevision string `json:"evergit_revision,omitempty"`
	// ConvertedRefs are the mirror's refs (SHA-1) at the last successful conversion, recorded as its
	// last step: a conversion is complete only if they match the mirror
	ConvertedRefs map[string]string `json:"converted_refs,omitempty"`
	// Partial records a conversion of the pinned refs only (see pinnedPrefix): the repository is
	// converted as far as the submodule commits superprojects record, and ConvertedRefs are those refs
	Partial bool `json:"partial,omitempty"`
	// Submodules records the SHA-256 decided for every submodule commit, by submodule as in
	// config.SyntheticGitlink (URL, or "path:<path>" for a gitlink without URL). A submodule commit
	// recorded under several submodules has the same SHA-256 under each.
	Submodules map[string]*submoduleState `json:"submodules,omitempty"`
	// MissingMergetagObjects maps objects that mergetags refer to but the repository lacks to a
	// merge recording one; they are translated to the synthetic SHA-256
	MissingMergetagObjects map[string]string `json:"missing_mergetag_objects,omitempty"`
}

type submoduleState struct {
	// Translated maps submodule commits to their real SHA-256
	Translated map[string]string `json:"translated,omitempty"`
	// Synthetic lists commits recorded with the synthetic SHA-256 (<sha1> + 24 zeros)
	Synthetic []syntheticDecision `json:"synthetic,omitempty"`
}

type syntheticDecision struct {
	Reason     string   `json:"reason"`
	ApprovedBy string   `json:"approved_by"` // the approval line that allowed it
	Commits    []string `json:"commits"`
}

// loadState reads a serving repository's state; os.ErrNotExist if it has none.
func loadState(repoPath string) (*repoState, error) {
	data, err := os.ReadFile(filepath.Join(repoPath, stateFile))
	if err != nil {
		return nil, err
	}
	var state repoState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("invalid %s in %s: %w", stateFile, repoPath, err)
	}
	return &state, nil
}

// save atomically writes the state into repoPath.
func (s *repoState) save(repoPath string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(repoPath, stateFile)
	if err := os.WriteFile(path+".tmp", append(data, '\n'), 0644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// gitlinkDecisions returns the SHA-256 recorded for each submodule commit. Real translations are
// only kept if the repository was converted with the current rules, since the submodules are
// rebuilt with them otherwise; synthetic ones do not depend on the rules.
func (s *repoState) gitlinkDecisions() (map[sha1ID]sha256ID, error) {
	decided := make(map[sha1ID]sha256ID)
	for _, sub := range s.Submodules {
		for _, d := range sub.decisions(s.ConversionVersion == conversionVersion) {
			oldID, newID, err := parseDecision(d[0], d[1])
			if err != nil {
				return nil, err
			}
			decided[oldID] = newID
		}
	}
	return decided, nil
}

// decisions lists a submodule's decisions as SHA-1 and SHA-256 pairs: the synthetic ones, and with
// current, the real translations.
func (sub *submoduleState) decisions(current bool) [][2]string {
	var pairs [][2]string
	for _, d := range sub.Synthetic {
		for _, c := range d.Commits {
			pairs = append(pairs, [2]string{c, c + syntheticSHA256Padding})
		}
	}
	if current {
		for oldHex, newHex := range sub.Translated {
			pairs = append(pairs, [2]string{oldHex, newHex})
		}
	}
	return pairs
}

// parseDecision parses a recorded decision.
func parseDecision(oldHex, newHex string) (sha1ID, sha256ID, error) {
	var newID sha256ID
	oldID, err := parseSHA1Hex(oldHex)
	if err != nil {
		return oldID, newID, err
	}
	if len(newHex) != 2*len(newID) {
		return oldID, newID, fmt.Errorf("invalid SHA-256 %q for submodule commit %s", newHex, oldHex)
	}
	if _, err := hex.Decode(newID[:], []byte(newHex)); err != nil {
		return oldID, newID, fmt.Errorf("invalid SHA-256 %q for submodule commit %s", newHex, oldHex)
	}
	return oldID, newID, nil
}

// recordGitlinks adds the decisions of a conversion, under the submodules that recorded them.
func (s *repoState) recordGitlinks(g *gitlinkResolution) {
	if s.Submodules == nil {
		s.Submodules = make(map[string]*submoduleState)
	}
	for _, submodule := range sortedKeys(g.recorded) {
		sub := s.Submodules[submodule]
		if sub == nil {
			sub = &submoduleState{}
			s.Submodules[submodule] = sub
		}
		sub.record(g.recorded[submodule], g.decided, g.synthetic[submodule])
	}
}

// record adds the decisions for commits a submodule records: real translations, and the synthetic
// ones approved as why. Commits the conversion did not reach are not recorded.
func (sub *submoduleState) record(commits []string, decided map[sha1ID]sha256ID, why syntheticDecision) {
	var synthetic []string
	for _, c := range commits {
		id, err := parseSHA1Hex(c)
		if err != nil {
			continue
		}
		newID, ok := decided[id]
		if !ok {
			continue
		}
		if newHex := hex.EncodeToString(newID[:]); newHex == c+syntheticSHA256Padding {
			synthetic = append(synthetic, c)
		} else {
			if sub.Translated == nil {
				sub.Translated = make(map[string]string)
			}
			sub.Translated[c] = newHex
		}
	}
	if len(synthetic) > 0 {
		slices.Sort(synthetic)
		sub.Synthetic = append(sub.Synthetic, syntheticDecision{Reason: why.Reason, ApprovedBy: why.ApprovedBy, Commits: synthetic})
	}
}

// recordMissingMergetags adds the mergetag objects a conversion found missing.
func (s *repoState) recordMissingMergetags(missing map[string]string) {
	for object, merge := range missing {
		if s.MissingMergetagObjects == nil {
			s.MissingMergetagObjects = make(map[string]string)
		}
		s.MissingMergetagObjects[object] = merge
	}
}

// newRepoState starts the state of a conversion, keeping the recorded decisions of the previous one.
func newRepoState(previous *repoState) *repoState {
	state := &repoState{ConversionVersion: conversionVersion, EvergitRevision: evergitRevision()}
	if previous == nil {
		return state
	}
	if previous.ConversionVersion == conversionVersion {
		state.recordMissingMergetags(previous.MissingMergetagObjects)
	}
	state.Submodules = make(map[string]*submoduleState, len(previous.Submodules))
	for submodule, sub := range previous.Submodules {
		kept := &submoduleState{Synthetic: sub.Synthetic}
		if previous.ConversionVersion == conversionVersion {
			kept.Translated = sub.Translated
		}
		state.Submodules[submodule] = kept
	}
	return state
}

// evergitRevision returns the VCS revision the binary was built from, if known.
func evergitRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	revision, modified := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision != "" && modified {
		revision += "-modified"
	}
	return revision
}

// currentState returns the state of a serving repository converted with the current rules, or nil.
func currentState(servingPath string) *repoState {
	state, err := loadState(servingPath)
	if err != nil || state.ConversionVersion != conversionVersion {
		return nil
	}
	return state
}
