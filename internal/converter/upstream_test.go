package converter

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"evergit/internal/config"
	"evergit/internal/resolver"
)

// newRedirectingUpstream serves upstream (a repository) over HTTPS with git's smart HTTP backend,
// behind a second server that redirects every request to target(backend URL). git and the redirect
// probe trust the test certificate.
func newRedirectingUpstream(t *testing.T, upstream string, target func(backendURL string) string) (redirector string) {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewTLSServer(&cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + filepath.Dir(upstream), "GIT_HTTP_EXPORT_ALL=1"},
	})
	t.Cleanup(backend.Close)
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target(backend.URL)+r.URL.RequestURI(), http.StatusFound)
	}))
	t.Cleanup(redirect.Close)

	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: backend.Certificate().Raw}), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", ca)
	t.Setenv("NO_PROXY", "*")
	client := *upstreamClient
	client.Transport = backend.Client().Transport
	previous := upstreamClient
	upstreamClient = &client
	t.Cleanup(func() { upstreamClient = previous })
	return redirect.URL
}

func redirectedRepoInfo(t *testing.T, remote string) *resolver.RepositoryInfo {
	root := t.TempDir()
	return &resolver.RepositoryInfo{
		Domain: "redirect.test", Owner: "owner", RepoName: "repo", RemoteURL: remote,
		MirrorPath:  filepath.Join(root, "mirrors", "repo.git"),
		ServingPath: filepath.Join(root, "repos", "repo.git"),
		BuildsPath:  filepath.Join(root, "builds", "repo.git"),
	}
}

func TestUpstreamRedirectToAllowedHostIsFollowed(t *testing.T) {
	up := newTestUpstream(t)
	bare := filepath.Join(t.TempDir(), "repo.git")
	gitRaw(t, "", nil, "clone", "-q", "--bare", up, bare)
	redirector := newRedirectingUpstream(t, bare, func(backend string) string { return backend })

	info := redirectedRepoInfo(t, redirector+"/repo.git")
	m := NewManager(0)
	m.SetConfig(&config.Config{AllowedHosts: []string{"127.0.0.1"}})
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("clone through an allowed redirect: %v", err)
	}

	// Fetches follow the redirect too
	commitToUpstream(t, up, "commit 1")
	gitRaw(t, up, nil, "push", "-q", bare, "main")
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatalf("fetch through an allowed redirect: %v", err)
	}
	if _, err := LookupID(info.ServingPath, "sha1", gitLine(t, up, nil, "rev-parse", "HEAD")); err != nil {
		t.Errorf("fetched commit not converted: %v", err)
	}
}

func TestUpstreamRedirectToOtherHostIsRefused(t *testing.T) {
	up := newTestUpstream(t)
	bare := filepath.Join(t.TempDir(), "repo.git")
	gitRaw(t, "", nil, "clone", "-q", "--bare", up, bare)
	// The same server under a name that is not allowed
	redirector := newRedirectingUpstream(t, bare, func(backend string) string { return strings.Replace(backend, "127.0.0.1", "localhost", 1) })

	info := redirectedRepoInfo(t, redirector+"/repo.git")
	m := NewManager(0)
	m.SetConfig(&config.Config{AllowedHosts: []string{"127.0.0.1"}})
	err := m.EnsureRepo(context.Background(), info, nil)
	if !errors.Is(err, ErrRedirectNotAllowed) || !strings.Contains(err.Error(), `"localhost"`) {
		t.Fatalf("clone redirected to a host that is not allowed = %v, want ErrRedirectNotAllowed naming it", err)
	}
}

func TestUpstreamRedirectToPlainHTTPIsRefused(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "repo.git")
	gitRaw(t, "", nil, "init", "-q", "--bare", bare)
	redirector := newRedirectingUpstream(t, bare, func(backend string) string { return strings.Replace(backend, "https://", "http://", 1) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := resolveRedirects(ctx, redirector+"/repo.git", []string{"127.0.0.1"}); !errors.Is(err, ErrRedirectNotAllowed) {
		t.Errorf("redirect from HTTPS to HTTP = %v, want ErrRedirectNotAllowed", err)
	}
}

func TestUpstreamRedirectProbeFailureReportsError(t *testing.T) {
	// Pointing at a port where nothing is listening to trigger an immediate connection failure
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := resolveRedirects(ctx, "https://127.0.0.1:54321/repo.git", []string{"127.0.0.1"})
	if err == nil || !strings.Contains(err.Error(), "probe upstream redirect for") {
		t.Fatalf("expected probe failure error, got: %v", err)
	}
}

func TestUpstreamRedirectMissingLocationReportsError(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Respond 302 without Location header
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)

	client := *upstreamClient
	client.Transport = server.Client().Transport
	previous := upstreamClient
	upstreamClient = &client
	t.Cleanup(func() { upstreamClient = previous })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := resolveRedirects(ctx, server.URL+"/repo.git", []string{"127.0.0.1"})
	if err == nil || !strings.Contains(err.Error(), "without Location header") {
		t.Fatalf("expected missing location error, got: %v", err)
	}
}
