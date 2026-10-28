package resolver

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"evergit/internal/config"
)

// validSegment restricts every path segment to characters that are safe both in a URL and on disk.
var validSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type RepositoryInfo struct {
	Domain      string // e.g. github.com
	Owner       string // e.g. ta or org/sub1/sub2; empty for hosts serving repositories at the top level
	RepoName    string // e.g. evergit (stripped of .git)
	RemoteURL   string // e.g. https://github.com/ta/evergit.git
	MirrorPath  string // e.g. <storageRoot>/mirrors/github.com/ta/evergit.git
	ServingPath string // e.g. <storageRoot>/repos/github.com/ta/evergit.git; symlink to the current build
	BuildsPath  string // e.g. <storageRoot>/builds/github.com/ta/evergit.git; holds full conversion builds
}

// Path returns the repository's path on its host, e.g. ta/evergit, or evergit without owner.
func (info *RepositoryInfo) Path() string {
	if info.Owner == "" {
		return info.RepoName
	}
	return info.Owner + "/" + info.RepoName
}

// caseInsensitiveHosts are known Git forge hosts where user/org names and repository names
// are strictly case-insensitive (e.g. "Torvalds/Linux" is identical to "torvalds/linux").
var caseInsensitiveHosts = []string{
	"github.com",
	"gitlab.com",
	"bitbucket.org",
}

func isCaseInsensitiveHost(domain string) bool {
	for _, host := range caseInsensitiveHosts {
		if domain == host || strings.HasSuffix(domain, "."+host) {
			return true
		}
	}
	return false
}

// ParsePath parses a raw Git path (e.g. "github/owner/repo.git", "github.com/org/sub/repo.git", or
// "boringssl.googlesource.com/boringssl" for a host serving repositories at the top level) and
// resolves it into detailed repository information based on the configured storage root. Only hosts
// listed in cfg.AllowedHosts are accepted.
func ParsePath(rawPath string, cfg *config.Config) (*RepositoryInfo, error) {
	storageRoot := cfg.StorageRoot

	path := strings.Trim(rawPath, "/")

	// Git clients might append .git
	path = strings.TrimSuffix(path, ".git")

	// Split the path into segments. No filepath.Clean: it keeps leading ".." on relative
	// paths, so traversal is rejected per segment instead.
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return nil, errors.New("invalid repository path: must be in the format 'domain/owner/repo', 'shorthand/owner/repo' or 'domain/repo'")
	}
	for i, part := range parts {
		if part == "." || part == ".." || !validSegment.MatchString(part) {
			return nil, fmt.Errorf("invalid repository path segment %q", part)
		}
		if i < len(parts)-1 && strings.HasSuffix(strings.ToLower(part), ".git") {
			return nil, fmt.Errorf("invalid repository path: non-final segment %q cannot end in .git", part)
		}
	}

	domain := strings.ToLower(parts[0])
	repoName := parts[len(parts)-1]
	ownerSegments := parts[1 : len(parts)-1]
	owner := strings.Join(ownerSegments, "/")

	// Handle shorthands
	if target, ok := cfg.Shorthands[domain]; ok {
		domain = target
	}

	// Only proxy explicitly allowed upstreams, so clients cannot point Evergit at internal hosts
	if !slices.Contains(cfg.AllowedHosts, domain) {
		return nil, fmt.Errorf("upstream host %q is not allowed", domain)
	}

	// Normalize owner and repo to lowercase for known case-insensitive hosts to avoid duplicate
	// mirrors, duplicate conversions, and repo lock bypassing.
	if isCaseInsensitiveHost(domain) {
		if owner == "" {
			return nil, fmt.Errorf("invalid repository path: repositories on %s are 'domain/owner/repo'", domain)
		}
		owner = strings.ToLower(owner)
		repoName = strings.ToLower(repoName)
	}

	info := &RepositoryInfo{Domain: domain, Owner: owner, RepoName: repoName}

	// Rebuild remote URL
	var remoteURL string
	if override, ok := cfg.UpstreamOverrides[domain]; ok {
		remoteURL = override
	} else {
		remoteURL = "https://" + domain + "/" + info.Path() + ".git"
	}

	// Define safe local paths
	mirrorPath := filepath.Join(storageRoot, "mirrors", domain, owner, repoName+".git")
	servingPath := filepath.Join(storageRoot, "repos", domain, owner, repoName+".git")
	buildsPath := filepath.Join(storageRoot, "builds", domain, owner, repoName+".git")
	if !isWithin(filepath.Join(storageRoot, "mirrors"), mirrorPath) || !isWithin(filepath.Join(storageRoot, "repos"), servingPath) {
		return nil, errors.New("invalid repository path: resolves outside of storage")
	}

	info.RemoteURL, info.MirrorPath, info.ServingPath, info.BuildsPath = remoteURL, mirrorPath, servingPath, buildsPath
	return info, nil
}

func isWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
