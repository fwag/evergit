package server

import (
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"strings"
	"text/template"

	"evergit/internal/config"
)

//go:embed setup.sh.tmpl
var setupScriptTemplateSource string

var setupScriptTmpl = template.Must(template.New("setup.sh").Parse(setupScriptTemplateSource))

type setupScriptData struct {
	HttpBase     string
	SshBase      string
	AllowedHosts string
}

func (s *HTTPServer) serveSetupScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	httpBase := s.cfg.PublicURL
	if httpBase == "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		httpBase = scheme + "://" + r.Host
	}
	httpBase = strings.TrimRight(httpBase, "/")

	sshBase := s.cfg.PublicSSHURL
	if sshBase == "" {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil && h != "" {
			host = h
		}
		sshPort := "2222"
		if _, p, err := net.SplitHostPort(s.cfg.SSHAddr); err == nil && p != "" {
			sshPort = p
		} else if strings.HasPrefix(s.cfg.SSHAddr, ":") {
			sshPort = strings.TrimPrefix(s.cfg.SSHAddr, ":")
		}
		sshBase = fmt.Sprintf("ssh://git@%s:%s", host, sshPort)
	}
	sshBase = strings.TrimRight(sshBase, "/")

	allowedHosts := s.cfg.AllowedHosts
	if len(allowedHosts) == 0 {
		allowedHosts = config.DefaultAllowedHosts
	}

	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}

	data := setupScriptData{
		HttpBase:     httpBase,
		SshBase:      sshBase,
		AllowedHosts: strings.Join(allowedHosts, " "),
	}

	_ = setupScriptTmpl.Execute(w, data)
}
