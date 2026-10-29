package config

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultAllowedHosts are the upstream hosts Evergit proxies when none are configured.
var DefaultAllowedHosts = []string{"github.com", "gitlab.com", "bitbucket.org"}

// DefaultShorthands are the default convenience prefix mappings to domains.
const DefaultShorthands = "github=github.com,gitlab=gitlab.com,bitbucket=bitbucket.org"

type Config struct {
	HTTPAddr       string
	SSHAddr        string
	PublicURL      string
	PublicSSHURL   string
	StorageRoot    string
	CacheTTL       time.Duration
	MaxConversions int
	MinFreeDiskGB  int
	// RequestWait is how long a request waits for a running sync or conversion before the current
	// copy is served, or a first conversion is reported as in progress; keep it below the proxy's
	// timeout. 0 waits until the conversion ends.
	RequestWait time.Duration
	// MaxConversionTime bounds a single sync or conversion
	MaxConversionTime time.Duration
	// MaxSubmoduleRepos bounds the number of submodule repositories a conversion may need
	MaxSubmoduleRepos int
	Debug             bool
	// AllowedHosts lists the upstream hosts clients may request; anything else is rejected
	AllowedHosts []string
	// Shorthands maps short prefix aliases to their target domains (e.g. "github" -> "github.com")
	Shorthands map[string]string
	// SyntheticGitlinksFile lists submodules approved for synthetic SHA-256s (see SyntheticGitlink);
	// it is read at every conversion, so approvals apply without a restart
	SyntheticGitlinksFile string
	// UpstreamOverrides maps a host to a local upstream path. Deliberately not exposed via flags
	// or environment variables; tests set it to serve fixtures without network access.
	UpstreamOverrides map[string]string
}

// DefaultMaxSubmoduleRepos is the maximum number of submodule repositories a conversion may need,
// e.g. 107 for qemu with edk2. It only stops runaway plans: exceeding it fails the conversion rather
// than changing any SHA-256.
const DefaultMaxSubmoduleRepos = 500

func Load() *Config {
	storageRoot := storageRootFromEnv()
	maxConversions := envInt("EVERGIT_MAX_CONVERSIONS", runtime.NumCPU(), 1)
	minFreeDiskGB := envInt("EVERGIT_MIN_FREE_DISK_GB", 1, 0)
	cacheTTL := envDuration("EVERGIT_CACHE_TTL", 30*time.Second, 0)
	requestWait := envDuration("EVERGIT_REQUEST_WAIT", 20*time.Second, 0)
	maxConversionTime := envDuration("EVERGIT_MAX_CONVERSION_TIME", 12*time.Hour, time.Nanosecond)
	maxSubmoduleRepos := envInt("EVERGIT_MAX_SUBMODULE_REPOS", DefaultMaxSubmoduleRepos, 0)

	debug := os.Getenv("EVERGIT_DEBUG") == "1" || strings.EqualFold(os.Getenv("EVERGIT_DEBUG"), "true")

	return &Config{
		HTTPAddr:       getEnv("EVERGIT_HTTP_ADDR", ":8080"),
		SSHAddr:        getEnv("EVERGIT_SSH_ADDR", ":2222"),
		PublicURL:      os.Getenv("EVERGIT_PUBLIC_URL"),
		PublicSSHURL:   os.Getenv("EVERGIT_PUBLIC_SSH_URL"),
		StorageRoot:    storageRoot,
		CacheTTL:       cacheTTL,
		MaxConversions: maxConversions,
		MinFreeDiskGB:  minFreeDiskGB,

		RequestWait:       requestWait,
		MaxConversionTime: maxConversionTime,
		MaxSubmoduleRepos: maxSubmoduleRepos,
		Debug:             debug,
		AllowedHosts:      ParseHostList(getEnv("EVERGIT_ALLOWED_HOSTS", strings.Join(DefaultAllowedHosts, ","))),
		Shorthands:        ParseShorthands(getEnv("EVERGIT_SHORTHANDS", DefaultShorthands)),

		SyntheticGitlinksFile: os.Getenv("EVERGIT_SYNTHETIC_GITLINKS"),
	}
}

// storageRootFromEnv returns the absolute storage root: EVERGIT_STORAGE_ROOT, systemd's
// STATE_DIRECTORY, or ./storage.
func storageRootFromEnv() string {
	storageRoot := os.Getenv("EVERGIT_STORAGE_ROOT")
	if storageRoot == "" {
		storageRoot = getEnv("STATE_DIRECTORY", "./storage")
	}
	storageRoot, _ = filepath.Abs(storageRoot)
	return storageRoot
}

// envInt returns an integer environment variable, or def if it is unset, invalid or below minimum.
func envInt(key string, def, minimum int) int {
	if val, err := strconv.Atoi(os.Getenv(key)); err == nil && val >= minimum {
		return val
	}
	return def
}

// envDuration returns a duration environment variable, or def if it is unset, invalid or below
// minimum.
func envDuration(key string, def, minimum time.Duration) time.Duration {
	if val, err := time.ParseDuration(os.Getenv(key)); err == nil && val >= minimum {
		return val
	}
	return def
}

func getEnv(key, defaultValue string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultValue
}

// ParseHostList splits a comma-separated host list, normalizing to lowercase.
func ParseHostList(list string) []string {
	var hosts []string
	for _, host := range strings.Split(list, ",") {
		if host = strings.ToLower(strings.TrimSpace(host)); host != "" {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// ParseShorthands parses a comma-separated list of alias=domain pairs.
func ParseShorthands(list string) map[string]string {
	shorthands := make(map[string]string)
	if list == "" || list == "none" || list == "off" {
		return shorthands
	}
	for _, pair := range strings.Split(list, ",") {
		alias, target, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		alias = strings.ToLower(strings.TrimSpace(alias))
		target = strings.ToLower(strings.TrimSpace(target))
		if alias != "" && target != "" {
			shorthands[alias] = target
		}
	}
	return shorthands
}

// FormatShorthands formats the shorthands map into a sorted comma-separated alias=domain list.
func FormatShorthands(shorthands map[string]string) string {
	if len(shorthands) == 0 {
		return "none"
	}
	var pairs []string
	for alias, domain := range shorthands {
		pairs = append(pairs, alias+"="+domain)
	}
	slices.Sort(pairs)
	return strings.Join(pairs, ", ")
}
