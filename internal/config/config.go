package config

import (
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	HTTPAddr    string
	SSHAddr     string
	StorageRoot string
	CacheTTL    time.Duration
}

func Load() *Config {
	storageRoot := os.Getenv("EVERGIT_STORAGE_ROOT")
	if storageRoot == "" {
		storageRoot = "./storage"
	}
	storageRoot, _ = filepath.Abs(storageRoot)

	return &Config{
		HTTPAddr:    getEnv("EVERGIT_HTTP_ADDR", ":8080"),
		SSHAddr:     getEnv("EVERGIT_SSH_ADDR", ":2222"),
		StorageRoot: storageRoot,
		CacheTTL:    5 * time.Minute,
	}
}

func getEnv(key, defaultValue string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultValue
}
