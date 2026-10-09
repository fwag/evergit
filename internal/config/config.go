package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// DefaultAllowedHosts are the upstream hosts Evergit proxies when none are configured.
var DefaultAllowedHosts = []string{"github.com", "gitlab.com", "bitbucket.org"}

// DefaultShorthands are the default convenience prefix mappings to domains.
const DefaultShorthands = "github=github.com,gitlab=gitlab.com,bitbucket=bitbucket.org"

type Config struct {
	HTTPAddr    string
	SSHAddr     string
	StorageRoot string
	CacheTTL    time.Duration
	// AllowedHosts lists the upstream hosts clients may request; anything else is rejected
	AllowedHosts []string
	// Shorthands maps short prefix aliases to their target domains (e.g. "github" -> "github.com")
	Shorthands map[string]string
	// UpstreamOverrides maps a host to a local upstream path. Deliberately not exposed via flags
	// or environment variables; tests set it to serve fixtures without network access.
	UpstreamOverrides map[string]string
}

func Load() *Config {
	storageRoot := os.Getenv("EVERGIT_STORAGE_ROOT")
	if storageRoot == "" {
		storageRoot = "./storage"
	}
	storageRoot, _ = filepath.Abs(storageRoot)

	return &Config{
		HTTPAddr:     getEnv("EVERGIT_HTTP_ADDR", ":8080"),
		SSHAddr:      getEnv("EVERGIT_SSH_ADDR", ":2222"),
		StorageRoot:  storageRoot,
		CacheTTL:     5 * time.Minute,
		AllowedHosts: ParseHostList(getEnv("EVERGIT_ALLOWED_HOSTS", strings.Join(DefaultAllowedHosts, ","))),
		Shorthands:   ParseShorthands(getEnv("EVERGIT_SHORTHANDS", DefaultShorthands)),
	}
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
