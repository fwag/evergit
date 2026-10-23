package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// SyntheticGitlink approves recording the commits of a submodule with the synthetic SHA-256
// (<sha1> + 24 zeros) when they cannot be converted: the submodule's host is not allowed, its URL is
// unusable, the recording commit's .gitmodules gives no URL, or the repository or commit no longer
// exists upstream. Without an approval such a conversion fails.
type SyntheticGitlink struct {
	// Superproject is the canonical repository (e.g. "gitlab.com/libvirt/libvirt"), or "*" for any
	Superproject string
	// Submodule is the URL as written in .gitmodules, or "path:<path>" for a gitlink without URL
	Submodule string
}

// ParseSyntheticGitlinks parses "<superproject> <submodule>" lines; blank lines and lines starting
// with '#' are ignored.
func ParseSyntheticGitlinks(r io.Reader) ([]SyntheticGitlink, error) {
	var entries []SyntheticGitlink
	scanner := bufio.NewScanner(r)
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sep := strings.IndexAny(line, " \t")
		if sep < 0 {
			return nil, fmt.Errorf("line %d: want \"<superproject> <submodule URL or path:<path>>\", got %q", n, line)
		}
		superproject, submodule := line[:sep], strings.TrimSpace(line[sep:])
		if submodule == "" || strings.ContainsAny(submodule, " \t") && !strings.HasPrefix(submodule, "path:") {
			return nil, fmt.Errorf("line %d: want \"<superproject> <submodule URL or path:<path>>\", got %q", n, line)
		}
		entries = append(entries, SyntheticGitlink{Superproject: superproject, Submodule: submodule})
	}
	return entries, scanner.Err()
}

// LoadSyntheticGitlinks reads approvals from path; an empty path approves nothing.
func LoadSyntheticGitlinks(path string) ([]SyntheticGitlink, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := ParseSyntheticGitlinks(f)
	if err != nil {
		return nil, fmt.Errorf("invalid synthetic gitlinks file %s: %w", path, err)
	}
	return entries, nil
}

// FindSyntheticApproval returns the entry approving synthetic SHA-256s for submodule of superproject.
func FindSyntheticApproval(entries []SyntheticGitlink, superproject, submodule string) (SyntheticGitlink, bool) {
	for _, e := range entries {
		if (e.Superproject == "*" || e.Superproject == superproject) && e.Submodule == submodule {
			return e, true
		}
	}
	return SyntheticGitlink{}, false
}

// String formats the entry as a line of the synthetic gitlinks file.
func (e SyntheticGitlink) String() string {
	return e.Superproject + " " + e.Submodule
}
