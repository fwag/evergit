package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/cgi"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"evergit/internal/config"
	"evergit/internal/converter"
	"evergit/internal/resolver"
)

type HTTPServer struct {
	cfg       *config.Config
	converter *converter.Manager
	handler   http.Handler
}

func NewHTTPServer(cfg *config.Config, conv *converter.Manager) (*HTTPServer, error) {
	// Find the git-http-backend path utilizing git --exec-path
	execPathCmd := exec.Command("git", "--exec-path")
	execPathCmd.Env = converter.GitEnv()
	gitExecPath, err := execPathCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to locate git exec-path: %w", err)
	}
	backendPath := filepath.Join(strings.TrimSpace(string(gitExecPath)), "git-http-backend")

	s := &HTTPServer{
		cfg:       cfg,
		converter: conv,
	}

	// Setup CGI handler
	cgiHandler := &cgi.Handler{
		Path: backendPath,
		Env: []string{
			"GIT_PROJECT_ROOT=" + filepath.Join(cfg.StorageRoot, "repos"),
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}

	// Define our smart routing and JIT middleware
	s.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[HTTP] Request: %s %s", r.Method, r.URL.Path)

		// Client setup script: /setup.sh or /install.sh
		if r.URL.Path == "/setup.sh" || r.URL.Path == "/install.sh" {
			s.serveSetupScript(w, r)
			return
		}

		// SHA-1 <-> SHA-256 id lookups: /<repo>/sha1/<id> and /<repo>/sha256/<id>
		if m := idLookupPath.FindStringSubmatch(r.URL.Path); m != nil {
			if info, err := resolver.ParsePath(m[1], cfg); err == nil {
				serveIDLookup(w, r.Context(), conv, info, m[2], m[3])
				return
			}
		}

		// Parse the repository info from the request path
		repoPath, ok := extractRepoPath(r.URL.Path)
		if !ok {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}

		info, err := resolver.ParsePath(repoPath, cfg)
		if err != nil {
			log.Printf("[HTTP] Path parse error: %v", err)
			http.Error(w, "Invalid repository path", http.StatusBadRequest)
			return
		}

		// Reject push attempts immediately before triggering any clone or conversion
		if r.URL.Query().Get("service") == "git-receive-pack" || strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			http.Error(w, "Evergit is read-only: push operations are not permitted", http.StatusForbidden)
			return
		}

		// Run JIT clone and conversion if necessary
		log.Printf("[HTTP] Ensuring repository %s is available & converted...", info.RepoName)
		if err := conv.EnsureRepo(r.Context(), info, nil); err != nil {
			writeConversionError(w, info, err)
			return
		}

		// Serve using git-http-backend cgi
		cgiHandler.ServeHTTP(w, r)
	})

	return s, nil
}

// writeConversionError reports a failed EnsureRepo. git clients print the body as "remote: ...",
// so known limitations are explained instead of reported as a generic failure.
func writeConversionError(w http.ResponseWriter, info *resolver.RepositoryInfo, err error) {
	log.Printf("[HTTP] JIT conversion failed for %s: %v", info.RepoName, err)
	if errors.Is(err, converter.ErrServerBusy) {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "Evergit: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if errors.Is(err, converter.ErrSubmoduleNotApproved) {
		http.Error(w, "Evergit: "+err.Error(), http.StatusForbidden)
		return
	}
	if errors.Is(err, converter.ErrSubmoduleUnavailable) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "Evergit: "+err.Error()+"\nThe repository is not served until its submodules can be converted; please retry later.", http.StatusServiceUnavailable)
		return
	}
	http.Error(w, "Internal Server Error (gitconv failure)", http.StatusInternalServerError)
}

var idLookupPath = regexp.MustCompile(`^/(.+)/(sha1|sha256)/([^/]+)$`)

// serveIDLookup answers with the id of the same object in the other hash format, as plain text.
func serveIDLookup(w http.ResponseWriter, ctx context.Context, conv *converter.Manager, info *resolver.RepositoryInfo, format, id string) {
	if err := conv.EnsureRepo(ctx, info, nil); err != nil {
		writeConversionError(w, info, err)
		return
	}

	translated, err := converter.LookupID(info.ServingPath, format, id)
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, translated)
	case errors.Is(err, converter.ErrIDInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, converter.ErrIDNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, converter.ErrIDAmbiguous):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		log.Printf("[HTTP] id lookup failed for %s: %v", info.RepoName, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (s *HTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *HTTPServer) ListenAndServe() error {
	log.Printf("Starting Evergit HTTP Server on %s", s.cfg.HTTPAddr)
	server := &http.Server{
		Addr:    s.cfg.HTTPAddr,
		Handler: s,
		// No WriteTimeout: large clones legitimately stream for a long time
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	return server.ListenAndServe()
}

// extractRepoPath extracts the repository path from the HTTP request path.
// Git smart HTTP requests have suffixes like /info/refs, /git-upload-pack, or /git-receive-pack.
func extractRepoPath(urlPath string) (string, bool) {
	urlPath = strings.TrimPrefix(urlPath, "/")
	if urlPath == "" {
		return "", false
	}

	// Standard Git endpoints
	suffixes := []string{
		"/info/refs",
		"/git-upload-pack",
		"/git-receive-pack",
	}

	for _, suffix := range suffixes {
		if strings.HasSuffix(urlPath, suffix) {
			repoPath := strings.TrimSuffix(urlPath, suffix)
			return repoPath, true
		}
	}

	// If no standard suffix matches, let's treat the entire path as the repo path
	return urlPath, true
}
