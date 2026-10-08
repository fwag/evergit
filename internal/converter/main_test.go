package converter

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain isolates git from the developer's global and system configuration, so results do not
// depend on settings such as init.defaultBranch, safe.directory or credential helpers.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "evergit-gitconfig-*")
	if err != nil {
		panic(err)
	}
	globalConfig := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(globalConfig, nil, 0644); err != nil {
		panic(err)
	}
	os.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
