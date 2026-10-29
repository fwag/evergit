package server_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

	convManager := converter.NewManager(cfg.CacheTTL)

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

		// git sends GIT_PROTOCOL through SSH for protocol v2, which the server honours
		lsRemote := exec.Command("git", "-c", "protocol.version=2", "ls-remote", "ssh://git@127.0.0.1/local.test/test/repo.git")
		lsRemote.Env = append(os.Environ(), "GIT_SSH_COMMAND="+sshCmd, "GIT_TRACE_PACKET=1")
		packets, err := lsRemote.CombinedOutput()
		if err != nil {
			t.Fatalf("ls-remote over SSH: %v\n%s", err, packets)
		}
		if !strings.Contains(string(packets), "< version 2") {
			t.Errorf("SSH server did not speak protocol v2:\n%s", packets)
		}
	})
}

func TestServeSetupScript(t *testing.T) {
	tempDir := t.TempDir()
	cfg := &config.Config{
		HTTPAddr:     "127.0.0.1:0",
		SSHAddr:      ":2222",
		StorageRoot:  tempDir,
		AllowedHosts: []string{"github.com", "gitlab.com"},
	}

	convManager := converter.NewManager(0)
	httpServer, err := server.NewHTTPServer(cfg, convManager)
	if err != nil {
		t.Fatalf("failed to create http server: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer listener.Close()

	srv := &http.Server{Handler: httpServer}
	go func() { _ = srv.Serve(listener) }() // returns when the server is closed
	defer srv.Close()

	baseURL := "http://" + listener.Addr().String()

	// 1. Test GET /setup.sh with dynamic Host fallback
	resp, err := http.Get(baseURL + "/setup.sh")
	if err != nil {
		t.Fatalf("GET /setup.sh failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/x-shellscript") {
		t.Errorf("expected text/x-shellscript, got: %q", ct)
	}

	scriptBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	scriptContent := string(scriptBytes)

	if !strings.Contains(scriptContent, baseURL) {
		t.Errorf("expected script to contain detected base URL %q, got:\n%s", baseURL, scriptContent)
	}
	if !strings.Contains(scriptContent, "ALLOWED_HOSTS=('github.com' 'gitlab.com')") {
		t.Errorf("expected script to contain allowed hosts, got:\n%s", scriptContent)
	}

	// 2. Test running script in bash with --dry-run --dir ~/tmp
	cmd := exec.Command("bash", "-s", "--", "--dry-run", "--dir", "~/tmp")
	cmd.Stdin = strings.NewReader(scriptContent)
	dryRunOut, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running script with --dry-run failed: %v\nOutput: %s", err, string(dryRunOut))
	}
	if !strings.Contains(string(dryRunOut), "Writing Evergit configuration to ") || !strings.Contains(string(dryRunOut), ".config/evergit/gitconfig") {
		t.Errorf("expected script to target ~/.config/evergit/gitconfig:\n%s", string(dryRunOut))
	}
	if !strings.Contains(string(dryRunOut), "git config --global includeIf.gitdir:") {
		t.Errorf("expected includeIf rule in dry-run output:\n%s", string(dryRunOut))
	}

	// 3. Test running script in bash with --dry-run --ssh
	cmdSSH := exec.Command("bash", "-s", "--", "--dry-run", "--ssh")
	cmdSSH.Stdin = strings.NewReader(scriptContent)
	dryRunSSHOut, err := cmdSSH.CombinedOutput()
	if err != nil {
		t.Fatalf("running script with --dry-run --ssh failed: %v\nOutput: %s", err, string(dryRunSSHOut))
	}
	if !strings.Contains(string(dryRunSSHOut), "ssh://git@") {
		t.Errorf("expected ssh dry-run output, got:\n%s", string(dryRunSSHOut))
	}

	// 4. Test running script in bash with --dry-run --remote upstream
	cmdRemote := exec.Command("bash", "-s", "--", "--dry-run", "--remote", "upstream")
	cmdRemote.Stdin = strings.NewReader(scriptContent)
	dryRunRemoteOut, err := cmdRemote.CombinedOutput()
	if err != nil {
		t.Fatalf("running script with --dry-run --remote failed: %v\nOutput: %s", err, string(dryRunRemoteOut))
	}
	if !strings.Contains(string(dryRunRemoteOut), "evergit.remote upstream") {
		t.Errorf("expected evergit.remote in dry-run output, got:\n%s", string(dryRunRemoteOut))
	}

	// 5. Test explicit PublicURL override
	cfg.PublicURL = "https://evergit.corp:8443"
	resp2, err := http.Get(baseURL + "/install.sh")
	if err != nil {
		t.Fatalf("GET /install.sh failed: %v", err)
	}
	defer resp2.Body.Close()

	scriptBytes2, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	scriptContent2 := string(scriptBytes2)
	if !strings.Contains(scriptContent2, `HTTP_BASE='https://evergit.corp:8443'`) {
		t.Errorf("expected script to contain PublicURL override, got:\n%s", scriptContent2)
	}
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

func TestSetupScriptRejectsShellInjection(t *testing.T) {
	cfg := &config.Config{SSHAddr: ":2222", StorageRoot: t.TempDir(), AllowedHosts: []string{"github.com"}}
	httpServer, err := server.NewHTTPServer(cfg, converter.NewManager(0))
	if err != nil {
		t.Fatal(err)
	}
	get := func(host string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/setup.sh", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		httpServer.ServeHTTP(rec, req)
		return rec
	}

	// Go's HTTP server accepts this Host header; rendered into the script it would run in the
	// victim's shell under "curl | bash"
	injection := "$(id;touch$IFS'pwned')"
	if rec := get(injection); rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), injection) {
		t.Errorf("Host %q: status %d, body %q; want 400 without the value", injection, rec.Code, rec.Body.String())
	}
	if rec := get("[::1]:8080"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "HTTP_BASE='http://[::1]:8080'") {
		t.Errorf("IPv6 Host: status %d, want 200 with the quoted base URL", rec.Code)
	}

	// A misconfigured public URL is refused too
	cfg.PublicURL, cfg.PublicSSHURL = "https://evergit.example/$(id)", "ssh://git@evergit.example:2222"
	if rec := get("evergit.example"); rec.Code != http.StatusInternalServerError {
		t.Errorf("unsafe -public-url: status %d, want 500", rec.Code)
	}

	// With both public URLs set the Host header is not used
	cfg.PublicURL = "https://evergit.example"
	rec := get(injection)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), injection) {
		t.Fatalf("public URLs set: status %d, want 200 without the Host header", rec.Code)
	}
	if out, err := exec.Command("bash", "-n", "-c", rec.Body.String()).CombinedOutput(); err != nil {
		t.Errorf("generated script is not valid bash: %v\n%s", err, out)
	}
}
