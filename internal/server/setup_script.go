package server

import (
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"text/template"

	"evergit/internal/config"
)

//go:embed setup.sh.tmpl
var setupScriptTemplateSource string

var setupScriptTmpl = template.Must(template.New("setup.sh").Funcs(template.FuncMap{"shq": shellQuote}).Parse(setupScriptTemplateSource))

type setupScriptData struct {
	HttpBase     string
	SshBase      string
	AllowedHosts []string
}

// requestHost is a host name or IP address with an optional port: without -public-url, the base URLs
// come from the request's Host header, which an attacker may influence (e.g. through a proxy or a
// poisoned cache), and Go accepts shell syntax such as "$(...)" in it.
var requestHost = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]+)(:[0-9]{1,5})?$`)

// scriptValue is what any value rendered into the script may contain. The script also embeds the
// base URL in the git aliases it installs, inside double quotes, so quoting the assignments alone
// would not keep "$", "`", quotes or backslashes from being executed later.
var scriptValue = regexp.MustCompile(`^[A-Za-z0-9._~:/@%+=,\[\]-]+$`)

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// publicHTTPBase returns the public base URL of the HTTP service: -public-url, or the request's.
func (s *HTTPServer) publicHTTPBase(r *http.Request) string {
	httpBase := s.cfg.PublicURL
	if httpBase == "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		httpBase = scheme + "://" + r.Host
	}
	return strings.TrimRight(httpBase, "/")
}

// publicSSHBase returns the public base URL of the SSH service: -public-ssh-url, or the request's
// host with the SSH listen port.
func (s *HTTPServer) publicSSHBase(r *http.Request) string {
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
	return strings.TrimRight(sshBase, "/")
}

func (s *HTTPServer) serveSetupScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	if (s.cfg.PublicURL == "" || s.cfg.PublicSSHURL == "") && !requestHost.MatchString(r.Host) {
		http.Error(w, "Evergit: invalid Host header for the setup script; the administrator can set -public-url and -public-ssh-url", http.StatusBadRequest)
		return
	}

	httpBase := s.publicHTTPBase(r)
	sshBase := s.publicSSHBase(r)

	allowedHosts := s.cfg.AllowedHosts
	if len(allowedHosts) == 0 {
		allowedHosts = config.DefaultAllowedHosts
	}

	for _, value := range append([]string{httpBase, sshBase}, allowedHosts...) {
		if !scriptValue.MatchString(value) {
			http.Error(w, fmt.Sprintf("Evergit: cannot generate the setup script, %q contains unsafe characters; check -public-url, -public-ssh-url and -allowed-hosts", value), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}

	data := setupScriptData{
		HttpBase:     httpBase,
		SshBase:      sshBase,
		AllowedHosts: allowedHosts,
	}

	_ = setupScriptTmpl.Execute(w, data)
}
