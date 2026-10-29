package converter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"evergit/internal/resolver"
)

// ErrRedirectNotAllowed is returned when an upstream redirects outside the allowed hosts, or to
// something else than a repository URL.
var ErrRedirectNotAllowed = errors.New("upstream redirect not allowed")

const maxUpstreamRedirects = 5

// upstreamClient finds where an upstream redirects to, without following; tests replace it.
var upstreamClient = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// gitUpstream runs git, with the arguments args returns for an upstream URL, against info's
// upstream. git never follows redirects: an allowed host could redirect it to an internal address.
// When the upstream redirects (e.g. git.savannah.gnu.org to https.git.savannah.gnu.org), the
// redirects are followed here instead, over HTTPS and only to allowed hosts, and git runs again
// with the final URL.
func (m *Manager) gitUpstream(ctx context.Context, info *resolver.RepositoryInfo, dir string, args func(remote string) []string) error {
	_, err := runCmdContext(ctx, upstreamTimeout, dir, "git", args(info.RemoteURL)...)
	if err == nil || !strings.Contains(err.Error(), "The requested URL returned error: 30") {
		return err
	}
	var allowedHosts []string
	if m.cfg != nil {
		allowedHosts = m.cfg.AllowedHosts
	}
	resolved, rerr := resolveRedirects(ctx, info.RemoteURL, allowedHosts)
	if rerr != nil {
		return rerr
	}
	if resolved == info.RemoteURL {
		return err
	}
	_, err = runCmdContext(ctx, upstreamTimeout, dir, "git", args(resolved)...)
	return err
}

// resolveRedirects follows the redirects of a repository URL's smart HTTP endpoint, over HTTPS and
// only to allowed hosts, and returns the final repository URL. A redirect to plain HTTP, as
// git.qemu.org's to http://gitlab.com/..., is followed to the same URL over HTTPS: nothing is ever
// fetched over HTTP.
func resolveRedirects(ctx context.Context, remote string, allowedHosts []string) (string, error) {
	current := strings.TrimSuffix(remote, "/")
	for hop := 0; hop <= maxUpstreamRedirects; hop++ {
		resp, err := probeUpstream(ctx, current)
		if err != nil {
			if hop == 0 {
				return "", fmt.Errorf("probe upstream redirect for %s: %w", current, err)
			}
			return current, nil // git reports the error itself
		}
		resp.Body.Close()
		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			return current, nil
		}
		if current, err = redirectTarget(current, resp, allowedHosts); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("%w: %s redirects more than %d times", ErrRedirectNotAllowed, remote, maxUpstreamRedirects)
}

// probeUpstream requests a repository URL's smart HTTP endpoint, retrying once on a transient error.
func probeUpstream(ctx context.Context, repoURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, repoURL+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "git/evergit")
	resp, err := upstreamClient.Do(req)
	if err == nil || ctx.Err() != nil {
		return resp, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(500 * time.Millisecond):
	}
	return upstreamClient.Do(req)
}

// redirectTarget returns the repository URL a redirect of current points to, over HTTPS, if it is a
// repository URL on an allowed host.
func redirectTarget(current string, resp *http.Response, allowedHosts []string) (string, error) {
	location, err := resp.Location()
	if err != nil {
		return "", fmt.Errorf("upstream %s returned redirect status %d without Location header: %w", current, resp.StatusCode, err)
	}
	base, isRepository := strings.CutSuffix(location.Path, "/info/refs")
	host := strings.ToLower(location.Hostname())
	switch {
	case location.Scheme != "https" && location.Scheme != "http" || !isRepository:
		return "", fmt.Errorf("%w: %s redirects to %s, which is not a repository URL", ErrRedirectNotAllowed, current, location.Redacted())
	case !slices.Contains(allowedHosts, host):
		return "", fmt.Errorf("%w: %s redirects to host %q, which is not in -allowed-hosts", ErrRedirectNotAllowed, current, host)
	}
	next := url.URL{Scheme: "https", Host: location.Host, Path: base}
	return next.String(), nil
}
