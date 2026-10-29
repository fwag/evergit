package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"evergit/internal/config"
	"evergit/internal/converter"
	"evergit/internal/server"
)

func main() {
	f := parseFlags()
	cfg := config.Load()
	f.apply(cfg)
	logConfig(cfg)
	checkSyntheticGitlinks(cfg)

	// Ensure Storage Root exists
	if err := os.MkdirAll(cfg.StorageRoot, 0755); err != nil {
		log.Fatalf("Failed to create storage root: %v", err)
	}

	lockFile, err := lockStorage(cfg.StorageRoot)
	if err != nil {
		log.Fatalf("Failed to lock storage root: %v", err)
	}
	// Also keeps the file referenced: a garbage-collected *os.File would close and release the lock
	defer lockFile.Close()

	convManager := newManager(cfg)

	// Start SSH Server
	sshServer, err := server.NewSSHServer(cfg, convManager)
	if err != nil {
		log.Fatalf("Failed to create SSH server: %v", err)
	}
	if _, err := sshServer.Start(); err != nil {
		log.Fatalf("Failed to start SSH server: %v", err)
	}

	// Start HTTP Server (Blocking)
	httpServer, err := server.NewHTTPServer(cfg, convManager)
	if err != nil {
		log.Fatalf("Failed to create HTTP server: %v", err)
	}
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatalf("HTTP server failed: %v", err)
	}
}

// flags are the command line flags, overriding the configuration from the environment when set.
type flags struct {
	httpAddr, sshAddr, publicURL, publicSSHURL, storageRoot string
	cacheTTL, requestWait, maxConversionTime                string
	maxConversions, minFreeDisk, maxSubmoduleRepos          int
	debug                                                   bool
	allowedHosts, syntheticGitlinks, shorthands             string
}

func parseFlags() *flags {
	f := &flags{}
	flag.StringVar(&f.httpAddr, "http", "", "HTTP listen address (e.g. :8080)")
	flag.StringVar(&f.sshAddr, "ssh", "", "SSH listen address (e.g. :2222)")
	flag.StringVar(&f.publicURL, "public-url", "", "Public base URL of this Evergit HTTP service (e.g. http://evergit.local:8080)")
	flag.StringVar(&f.publicSSHURL, "public-ssh-url", "", "Public base URL of this Evergit SSH service (e.g. ssh://git@evergit.local:2222)")
	flag.StringVar(&f.storageRoot, "storage", "", "Storage root directory")
	flag.StringVar(&f.cacheTTL, "ttl", "", "Cache TTL (e.g. 30s, 5m, 1h, 0 to disable cache; default: 30s)")
	flag.IntVar(&f.maxConversions, "max-conversions", 0, "Maximum concurrent JIT conversions (0 = CPU count)")
	flag.IntVar(&f.minFreeDisk, "min-free-disk", -1, "Minimum free disk space in GB required before cloning new mirrors (default: 1)")
	flag.StringVar(&f.requestWait, "request-wait", "", "How long a request waits for a running conversion before serving the current copy or answering 503; keep below the proxy timeout (default: 20s, 0 to wait for the end)")
	flag.StringVar(&f.maxConversionTime, "max-conversion-time", "", "Maximum duration of a single sync or conversion (default: 12h)")
	flag.IntVar(&f.maxSubmoduleRepos, "max-submodule-repos", -1, fmt.Sprintf("Maximum number of submodule repositories a conversion may need (default: %d)", config.DefaultMaxSubmoduleRepos))
	flag.BoolVar(&f.debug, "debug", false, "Enable debug logging")
	flag.StringVar(&f.allowedHosts, "allowed-hosts", "", "Comma-separated upstream hosts clients may request (default: github.com,gitlab.com,bitbucket.org)")
	flag.StringVar(&f.syntheticGitlinks, "synthetic-gitlinks", "", "File of \"<superproject> <submodule>\" lines approving synthetic SHA-256s for submodules that cannot be converted")
	flag.StringVar(&f.shorthands, "shorthands", "", "Comma-separated domain shorthand mappings in alias=domain format (default: github=github.com,gitlab=gitlab.com,bitbucket=bitbucket.org)")
	flag.Parse()
	return f
}

// apply overrides the configuration with the flags that are set, exiting on invalid values.
func (f *flags) apply(cfg *config.Config) {
	setString(&cfg.HTTPAddr, f.httpAddr)
	setString(&cfg.SSHAddr, f.sshAddr)
	setString(&cfg.PublicURL, f.publicURL)
	setString(&cfg.PublicSSHURL, f.publicSSHURL)
	setString(&cfg.SyntheticGitlinksFile, f.syntheticGitlinks)
	if f.storageRoot != "" {
		absPath, err := filepath.Abs(f.storageRoot)
		if err != nil {
			log.Fatalf("Failed to resolve absolute path of storage %q: %v", f.storageRoot, err)
		}
		cfg.StorageRoot = absPath
	}
	if f.cacheTTL != "" {
		cfg.CacheTTL = mustDuration("TTL duration", f.cacheTTL, 0)
	}
	if f.requestWait != "" {
		cfg.RequestWait = mustDuration("request wait", f.requestWait, 0)
	}
	if f.maxConversionTime != "" {
		cfg.MaxConversionTime = mustDuration("maximum conversion time", f.maxConversionTime, time.Nanosecond)
	}
	if f.maxConversions > 0 {
		cfg.MaxConversions = f.maxConversions
	}
	if f.minFreeDisk >= 0 {
		cfg.MinFreeDiskGB = f.minFreeDisk
	}
	if f.maxSubmoduleRepos >= 0 {
		cfg.MaxSubmoduleRepos = f.maxSubmoduleRepos
	}
	cfg.Debug = cfg.Debug || f.debug
	if f.allowedHosts != "" {
		cfg.AllowedHosts = config.ParseHostList(f.allowedHosts)
	}
	if f.shorthands != "" {
		cfg.Shorthands = config.ParseShorthands(f.shorthands)
	}
}

// setString sets a configuration value to a flag's value, if the flag is set.
func setString(value *string, flagValue string) {
	if flagValue != "" {
		*value = flagValue
	}
}

// mustDuration parses a duration flag of at least minimum, exiting if it is invalid.
func mustDuration(what, value string, minimum time.Duration) time.Duration {
	d, err := time.ParseDuration(value)
	if err != nil || d < minimum {
		kind := "non-negative"
		if minimum > 0 {
			kind = "positive"
		}
		log.Fatalf("Invalid %s %q: must be a %s duration", what, value, kind)
	}
	return d
}

func logConfig(cfg *config.Config) {
	log.Printf("Initializing Evergit Daemon...")
	log.Printf("Storage Root:    %s", cfg.StorageRoot)
	log.Printf("Cache TTL:       %v", cfg.CacheTTL)
	log.Printf("Max conversions: %d", cfg.MaxConversions)
	log.Printf("Min free disk:   %d GB", cfg.MinFreeDiskGB)
	log.Printf("Request wait:    %v", cfg.RequestWait)
	log.Printf("Max conversion:  %v", cfg.MaxConversionTime)
	log.Printf("Submodule repos: %d", cfg.MaxSubmoduleRepos)
	log.Printf("Debug:           %v", cfg.Debug)
	if cfg.PublicURL != "" {
		log.Printf("Public URL:      %s", cfg.PublicURL)
	}
	if cfg.PublicSSHURL != "" {
		log.Printf("Public SSH URL:  %s", cfg.PublicSSHURL)
	}
	log.Printf("Allowed hosts:   %s", strings.Join(cfg.AllowedHosts, ", "))
	log.Printf("Shorthands:      %s", config.FormatShorthands(cfg.Shorthands))
}

// checkSyntheticGitlinks loads the approvals file at startup, so a mistake shows immediately: it is
// read again at every conversion.
func checkSyntheticGitlinks(cfg *config.Config) {
	if cfg.SyntheticGitlinksFile == "" {
		return
	}
	approvals, err := config.LoadSyntheticGitlinks(cfg.SyntheticGitlinksFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
		log.Printf("Synthetic gitlinks: %s (does not exist yet, approves nothing)", cfg.SyntheticGitlinksFile)
	case err != nil:
		log.Fatalf("Failed to load synthetic gitlinks: %v", err)
	default:
		log.Printf("Synthetic gitlinks: %s (%d approvals)", cfg.SyntheticGitlinksFile, len(approvals))
	}
}

func newManager(cfg *config.Config) *converter.Manager {
	m := converter.NewManager(cfg.CacheTTL, cfg.MaxConversions)
	m.SetConfig(cfg)
	m.SetMinFreeDisk(uint64(cfg.MinFreeDiskGB) * 1024 * 1024 * 1024)
	m.SetExecTimeout(cfg.MaxConversionTime)
	m.SetMaxSubmoduleRepos(cfg.MaxSubmoduleRepos)
	m.SetDebug(cfg.Debug)
	return m
}

// lockStorage takes an exclusive lock on the storage root for the process lifetime. Repository
// locks only coordinate within one process, so a second instance on the same storage would race.
func lockStorage(root string) (*os.File, error) {
	path := filepath.Join(root, "evergit.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}
	// LOCK_EX: exclusive lock, LOCK_NB: non blocking
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s is locked, is another Evergit instance using this storage? (%w)", path, err)
	}
	return f, nil
}
