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
	Owner       string // e.g. ta or org/sub1/sub2
	RepoName    string // e.g. evergit (stripped of .git)
	RemoteURL   string // e.g. https://github.com/ta/evergit.git
	MirrorPath  string // e.g. <storageRoot>/mirrors/github.com/ta/evergit.git
	ServingPath string // e.g. <storageRoot>/repos/github.com/ta/evergit.git
}

// ParsePath parses a raw Git path (e.g. "github/owner/repo.git" or "github.com/org/sub/repo.git")
// and resolves it into detailed repository information based on the configured storage root.
// Only hosts listed in cfg.AllowedHosts are accepted.
func ParsePath(rawPath string, cfg *config.Config) (*RepositoryInfo, error) {
	storageRoot := cfg.StorageRoot

	path := strings.Trim(rawPath, "/")

	// Git clients might append .git
	path = strings.TrimSuffix(path, ".git")

	// Split the path into segments. No filepath.Clean: it keeps leading ".." on relative
	// paths, so traversal is rejected per segment instead.
	parts := strings.Split(path, "/")
	if len(parts) < 3 {
		return nil, errors.New("invalid repository path: must be in the format 'domain/owner/repo' or 'shorthand/owner/repo'")
	}
	for _, part := range parts {
		if part == "." || part == ".." || !validSegment.MatchString(part) {
			return nil, fmt.Errorf("invalid repository path segment %q", part)
		}
	}

	domain := strings.ToLower(parts[0])
	repoName := parts[len(parts)-1]
	ownerSegments := parts[1 : len(parts)-1]
	owner := strings.Join(ownerSegments, "/")

	// Handle shorthands
	switch domain {
	case "github":
		domain = "github.com"
	case "gitlab":
		domain = "gitlab.com"
	case "bitbucket":
		domain = "bitbucket.org"
	}

	// Only proxy explicitly allowed upstreams, so clients cannot point Evergit at internal hosts
	if !slices.Contains(cfg.AllowedHosts, domain) {
		return nil, fmt.Errorf("upstream host %q is not allowed", domain)
	}

	// Rebuild remote URL
	var remoteURL string
	if override, ok := cfg.UpstreamOverrides[domain]; ok {
		remoteURL = override
	} else {
		remoteURL = "https://" + domain + "/" + owner + "/" + repoName + ".git"
	}

	// Define safe local paths
	mirrorPath := filepath.Join(storageRoot, "mirrors", domain, owner, repoName+".git")
	servingPath := filepath.Join(storageRoot, "repos", domain, owner, repoName+".git")
	if !isWithin(filepath.Join(storageRoot, "mirrors"), mirrorPath) || !isWithin(filepath.Join(storageRoot, "repos"), servingPath) {
		return nil, errors.New("invalid repository path: resolves outside of storage")
	}

	return &RepositoryInfo{
		Domain:      domain,
		Owner:       owner,
		RepoName:    repoName,
		RemoteURL:   remoteURL,
		MirrorPath:  mirrorPath,
		ServingPath: servingPath,
	}, nil
}

func isWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
