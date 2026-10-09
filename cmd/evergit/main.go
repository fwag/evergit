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
	storageRoot := flag.String("storage", "", "Storage root directory")
	cacheTTLStr := flag.String("ttl", "5m", "Cache TTL (e.g. 5m, 1h)")
	allowedHosts := flag.String("allowed-hosts", "", "Comma-separated upstream hosts clients may request (default: github.com,gitlab.com,bitbucket.org)")
	flag.Parse()

	cfg := config.Load()

	// Override config with flags if provided
	if *httpAddr != "" {
		cfg.HTTPAddr = *httpAddr
	}
	if *sshAddr != "" {
		cfg.SSHAddr = *sshAddr
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
		if ttl <= 0 {
			log.Fatalf("Invalid TTL duration %q: must be positive", *cacheTTLStr)
		}
		cfg.CacheTTL = ttl
	}

	if *allowedHosts != "" {
		cfg.AllowedHosts = config.ParseHostList(*allowedHosts)
	}

	log.Printf("Initializing Evergit Daemon...")
	log.Printf("Storage Root: %s", cfg.StorageRoot)
	log.Printf("Cache TTL:    %v", cfg.CacheTTL)
	log.Printf("Allowed hosts: %s", strings.Join(cfg.AllowedHosts, ", "))

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

	convManager := converter.NewManager(cfg.CacheTTL)

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
