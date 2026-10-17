package main

import (
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
	// CLI Flags
	httpAddr := flag.String("http", "", "HTTP listen address (e.g. :8080)")
	sshAddr := flag.String("ssh", "", "SSH listen address (e.g. :2222)")
	publicURL := flag.String("public-url", "", "Public base URL of this Evergit HTTP service (e.g. http://evergit.local:8080)")
	publicSSHURL := flag.String("public-ssh-url", "", "Public base URL of this Evergit SSH service (e.g. ssh://git@evergit.local:2222)")
	storageRoot := flag.String("storage", "", "Storage root directory")
	cacheTTLStr := flag.String("ttl", "", "Cache TTL (e.g. 30s, 5m, 1h, 0 to disable cache; default: 30s)")
	maxConversionsFlag := flag.Int("max-conversions", 0, "Maximum concurrent JIT conversions (0 = CPU count)")
	minFreeDiskFlag := flag.Int("min-free-disk", -1, "Minimum free disk space in GB required before cloning new mirrors (default: 1)")
	debugFlag := flag.Bool("debug", false, "Enable debug mode and fast-import statistics")
	allowedHosts := flag.String("allowed-hosts", "", "Comma-separated upstream hosts clients may request (default: github.com,gitlab.com,bitbucket.org)")
	shorthands := flag.String("shorthands", "", "Comma-separated domain shorthand mappings in alias=domain format (default: github=github.com,gitlab=gitlab.com,bitbucket=bitbucket.org)")
	flag.Parse()

	cfg := config.Load()

	// Override config with flags if provided
	if *httpAddr != "" {
		cfg.HTTPAddr = *httpAddr
	}
	if *sshAddr != "" {
		cfg.SSHAddr = *sshAddr
	}
	if *publicURL != "" {
		cfg.PublicURL = *publicURL
	}
	if *publicSSHURL != "" {
		cfg.PublicSSHURL = *publicSSHURL
	}
	if *storageRoot != "" {
		absPath, err := filepath.Abs(*storageRoot)
		if err != nil {
			log.Fatalf("Failed to resolve absolute path of storage %q: %v", *storageRoot, err)
		}
		cfg.StorageRoot = absPath
	}
	if *cacheTTLStr != "" {
		ttl, err := time.ParseDuration(*cacheTTLStr)
		if err != nil {
			log.Fatalf("Invalid TTL duration %q: %v", *cacheTTLStr, err)
		}
		if ttl < 0 {
			log.Fatalf("Invalid TTL duration %q: must be non-negative", *cacheTTLStr)
		}
		cfg.CacheTTL = ttl
	}

	if *maxConversionsFlag > 0 {
		cfg.MaxConversions = *maxConversionsFlag
	}
	if *minFreeDiskFlag >= 0 {
		cfg.MinFreeDiskGB = *minFreeDiskFlag
	}
	if *debugFlag {
		cfg.Debug = true
	}

	if *allowedHosts != "" {
		cfg.AllowedHosts = config.ParseHostList(*allowedHosts)
	}
	if *shorthands != "" {
		cfg.Shorthands = config.ParseShorthands(*shorthands)
	}

	log.Printf("Initializing Evergit Daemon...")
	log.Printf("Storage Root:    %s", cfg.StorageRoot)
	log.Printf("Cache TTL:       %v", cfg.CacheTTL)
	log.Printf("Max conversions: %d", cfg.MaxConversions)
	log.Printf("Min free disk:   %d GB", cfg.MinFreeDiskGB)
	log.Printf("Debug:           %v", cfg.Debug)
	if cfg.PublicURL != "" {
		log.Printf("Public URL:      %s", cfg.PublicURL)
	}
	if cfg.PublicSSHURL != "" {
		log.Printf("Public SSH URL:  %s", cfg.PublicSSHURL)
	}
	log.Printf("Allowed hosts:   %s", strings.Join(cfg.AllowedHosts, ", "))
	log.Printf("Shorthands:      %s", config.FormatShorthands(cfg.Shorthands))

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

	convManager := converter.NewManager(cfg.CacheTTL, cfg.MaxConversions)
	convManager.SetConfig(cfg)
	convManager.SetMinFreeDisk(uint64(cfg.MinFreeDiskGB) * 1024 * 1024 * 1024)
	convManager.SetDebug(cfg.Debug)

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
