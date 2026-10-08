package server

import (
	"fmt"
	"log"
	"net/http"
	"net/http/cgi"
	"os/exec"
	"path/filepath"
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
	gitExecPath, err := exec.Command("git", "--exec-path").Output()
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

		// Run JIT clone and conversion if necessary
		log.Printf("[HTTP] Ensuring repository %s is available & converted...", info.RepoName)
		if err := conv.EnsureRepo(info, nil); err != nil {
			log.Printf("[HTTP] JIT conversion failed for %s: %v", info.RepoName, err)
			http.Error(w, "Internal Server Error (gitconv failure)", http.StatusInternalServerError)
			return
		}

		// Hijack to serve loose-object-idx directly
		if strings.HasSuffix(r.URL.Path, "/objects/loose-object-idx") {
			idxPath := filepath.Join(info.ServingPath, "objects", "loose-object-idx")
			http.ServeFile(w, r, idxPath)
			return
		}

		// Serve using git-http-backend cgi
		cgiHandler.ServeHTTP(w, r)
	})

	return s, nil
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
		"/objects/loose-object-idx",
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
