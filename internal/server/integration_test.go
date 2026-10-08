package server_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"evergit/internal/config"
	"evergit/internal/converter"
	"evergit/internal/server"
)

func TestIntegration(t *testing.T) {
	// Create a temp workspace for the entire test
	tempDir, err := os.MkdirTemp("", "evergit-integration-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// 1. Create a dummy SHA1 upstream repository
	upstreamPath := filepath.Join(tempDir, "upstream.git")
	err = os.MkdirAll(upstreamPath, 0755)
	if err != nil {
		t.Fatalf("failed to create upstream dir: %v", err)
	}

	_, err = runCmd(upstreamPath, "git", "init", "--initial-branch=main", "--object-format=sha1")
	if err != nil {
		_, err = runCmd(upstreamPath, "git", "init", "--object-format=sha1")
		if err != nil {
			t.Fatalf("failed to init upstream SHA1 repo: %v", err)
		}
	}

	// Configure git user
	_, _ = runCmd(upstreamPath, "git", "config", "user.name", "Test User")
	_, _ = runCmd(upstreamPath, "git", "config", "user.email", "test@example.com")

	// Create and commit a file
	testFile := filepath.Join(upstreamPath, "file.txt")
	err = os.WriteFile(testFile, []byte("evergit is awesome"), 0644)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	_, err = runCmd(upstreamPath, "git", "add", "file.txt")
	if err != nil {
		t.Fatalf("failed to add file: %v", err)
	}

	_, err = runCmd(upstreamPath, "git", "commit", "-m", "initial commit on sha1")
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	// 2. Configure and start Evergit Servers
	cfg := &config.Config{
		HTTPAddr:          "127.0.0.1:0",
		SSHAddr:           "127.0.0.1:0",
		StorageRoot:       filepath.Join(tempDir, "storage"),
		CacheTTL:          5 * time.Minute,
		AllowedHosts:      []string{"local.test"},
		UpstreamOverrides: map[string]string{"local.test": upstreamPath},
	}

	convManager := converter.NewManager(cfg.StorageRoot, cfg.CacheTTL)

	// Start SSH Server
	sshServer, err := server.NewSSHServer(cfg, convManager)
	if err != nil {
		t.Fatalf("failed to create SSH server: %v", err)
	}
	sshListener, err := sshServer.Start()
	if err != nil {
		t.Fatalf("failed to start SSH server: %v", err)
	}
	defer sshListener.Close()

	// Start HTTP Server
	httpServer, err := server.NewHTTPServer(cfg, convManager)
	if err != nil {
		t.Fatalf("failed to create HTTP server: %v", err)
	}

	httpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen on HTTP: %v", err)
	}
	cfg.HTTPAddr = httpListener.Addr().String()
	defer httpListener.Close()

	go func() {
		_ = http.Serve(httpListener, httpServer)
	}()

	// 3. Perform HTTP Clone and verify
	t.Run("HTTP Clone JIT Conversion", func(t *testing.T) {
		cloneHttpPath := filepath.Join(tempDir, "clone-http")
		_, err := runCmd("", "git", "clone", fmt.Sprintf("http://%s/local.test/test/repo.git", cfg.HTTPAddr), cloneHttpPath)
		if err != nil {
			t.Fatalf("failed to clone over HTTP: %v", err)
		}

		// Verify format of cloned repo is sha256
		format, err := runCmd(cloneHttpPath, "git", "rev-parse", "--show-object-format")
		if err != nil {
			t.Fatalf("failed to check cloned format: %v", err)
		}
		if format != "sha256" {
			t.Errorf("expected format sha256, got %q", format)
		}

		// Verify commit log matches
		logOut, err := runCmd(cloneHttpPath, "git", "log", "--oneline")
		if err != nil {
			t.Fatalf("failed to run git log: %v", err)
		}
		if !strings.Contains(logOut, "initial commit on sha1") {
			t.Errorf("missing expected commit in logs: %q", logOut)
		}
	})

	// 4. Perform SSH Clone and verify
	t.Run("SSH Clone JIT Conversion", func(t *testing.T) {
		// Generate client key for authentication
		clientKeyPath := filepath.Join(tempDir, "client_key")
		generateSSHClientKey(t, clientKeyPath)

		// Parse the host and port of SSH server
		_, sshPort, err := net.SplitHostPort(cfg.SSHAddr)
		if err != nil {
			t.Fatalf("failed to parse SSH address: %v", err)
		}

		sshCmd := fmt.Sprintf("ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i %s -p %s", clientKeyPath, sshPort)

		cloneSshPath := filepath.Join(tempDir, "clone-ssh")
		_, err = runCmdWithEnv("", []string{"GIT_SSH_COMMAND=" + sshCmd}, "git", "clone", "ssh://git@127.0.0.1/local.test/test/repo.git", cloneSshPath)
		if err != nil {
			t.Fatalf("failed to clone over SSH: %v", err)
		}

		// Verify format of cloned repo is sha256
		format, err := runCmd(cloneSshPath, "git", "rev-parse", "--show-object-format")
		if err != nil {
			t.Fatalf("failed to check cloned format: %v", err)
		}
		if format != "sha256" {
			t.Errorf("expected format sha256, got %q", format)
		}

		// Verify commit log matches
		logOut, err := runCmd(cloneSshPath, "git", "log", "--oneline")
		if err != nil {
			t.Fatalf("failed to run git log: %v", err)
		}
		if !strings.Contains(logOut, "initial commit on sha1") {
			t.Errorf("missing expected commit in logs: %q", logOut)
		}
	})
}

func runCmd(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("cmd %s %v failed in %s: %w (stderr: %q)", name, args, dir, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func runCmdWithEnv(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("cmd %s %v failed in %s: %w (stderr: %q)", name, args, dir, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func generateSSHClientKey(t *testing.T, path string) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate SSH key: %v", err)
	}

	privateKeyPEM := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatalf("failed to open key file: %v", err)
	}
	defer f.Close()

	if err := pem.Encode(f, privateKeyPEM); err != nil {
		t.Fatalf("failed to encode key: %v", err)
	}
}
