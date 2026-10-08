package config

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultAllowedHosts are the upstream hosts Evergit proxies when none are configured.
var DefaultAllowedHosts = []string{"github.com", "gitlab.com", "bitbucket.org"}

type Config struct {
	HTTPAddr    string
	SSHAddr     string
	StorageRoot string
	CacheTTL    time.Duration
	// AllowedHosts lists the upstream hosts clients may request; anything else is rejected
	AllowedHosts []string
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
