# Evergit Security & Infrastructure Hardening TODO

This checklist tracks security, rate-limiting, and hardening improvements across Evergit and its surrounding edge infrastructure (HAProxy).

---

## 0. Known Vulnerabilities (from security review)

- [ ] **[High] Nested repository paths can corrupt existing mirrors**
  - [ ] `resolver.ParsePath` accepts `.git` in non-final segments, so `/github/torvalds/linux.git/commondir/x` resolves to `mirrors/github.com/torvalds/linux.git/commondir/x.git`.
  - [ ] `syncMirror` runs `os.MkdirAll(filepath.Dir(MirrorPath))` before cloning, creating a `commondir` directory inside the real mirror; git then fails with `failed to read .../commondir: Is a directory`.
  - [ ] The broken mirror still passes `looksLikeBareRepo`, so it is never repaired: after the TTL `syncMirror` fails and the repo is unservable until manual cleanup. The request also takes a different repo lock.
  - [ ] Fix: reject any non-final segment ending in `.git`, plus a resolver test.

- [ ] **[Medium] Force-pushed history is served publicly**
  - [ ] `refs/evergit-backups/*` reaches the serving repo (`fast-export --all`, and the explicit refspec in `syncNativeSHA256`), so clients can fetch commits that upstream removed, e.g. leaked secrets or DMCA takedowns.
  - [ ] Decide: keep backups in the mirror only and stop serving them, or document the behavior explicitly.

- [ ] **[Low] Reject `/git-receive-pack` before `EnsureRepo`**
  - [ ] Push is currently refused only by `git-http-backend` (no `REMOTE_USER`), after a sync/conversion has already been triggered. Return `403` immediately.

- [ ] **[Low] Disable upstream HTTP redirects**
  - [ ] git follows the initial redirect (`http.followRedirects=initial`), so a self-hosted allowed host could redirect Evergit to an internal address. Pass `-c http.followRedirects=false` on clone/fetch.

- [ ] **[Low] Log injection via SSH command**
  - [ ] `executeGitCommand` logs `rawCmd` with `%s`; a client can forge log lines with embedded newlines. Use `%q`.

- [ ] **[Low] `LookupID` scans the whole `loose-object-idx` per request**
  - [ ] Expensive on very large repositories; consider an index or sorted file with binary search.

---

## 1. Evergit In-App Protections (Resource-Aware Controls)

Because HAProxy cannot inspect Git conversion costs or encrypted SSH application state, Evergit must guard its own internal resources.

- [ ] **Global JIT Conversion Concurrency Limiter**
  - [ ] Implement a global semaphore (e.g., using `golang.org/x/sync/semaphore`) to bound simultaneous `git-fast-export | git-fast-import` pipelines across all repositories.
  - [ ] Default concurrency cap to `runtime.NumCPU()` (or expose via `-max-conversions` flag).
  - [ ] Queue incoming conversions with timeout rather than spawning unbounded subprocesses.

- [ ] **Graceful Load Shedding & Informative Errors**
  - [ ] For HTTP: return `HTTP 429 Too Many Requests` or `HTTP 503 Service Unavailable` with `Retry-After` header when conversion queues are saturated.
  - [ ] For SSH: write user-friendly rejection messages to `stderr` channel (e.g., `error: server busy, please retry later`) before closing sessions.

- [ ] **Disk Space Safeguards & Storage Quotas**
  - [ ] Inspect available storage using `syscall.Statfs` before initiating a new upstream mirror clone.
  - [ ] Define minimum free disk threshold (e.g., 5 GB or 10% disk capacity).
  - [ ] Abort new clones with clear diagnostic logging when capacity threshold is breached.

- [ ] **Cache Maintenance & Pruning**
  - [ ] Implement background LRU or TTL-based garbage collection for stale cached mirrors under `storage/mirrors/` and `storage/builds/`.
  - [ ] Prevent runaway disk consumption from one-off repository clones.

- [ ] **Case-Insensitive Path Normalization**
  - [ ] `github/Torvalds/Linux` and `github/torvalds/linux` currently produce separate mirrors and conversions, multiplying resource usage and bypassing the per-repo lock.
  - [ ] Lowercase owner and repo for hosts known to be case-insensitive (GitHub, GitLab, Bitbucket).

- [ ] **SSH Session Limits**
  - [ ] Any key is accepted and a session that never sends `exec` is held forever: add an idle timeout for sessions awaiting a request.
  - [ ] Cap concurrent connections and channels per connection.

---

## 2. Edge Reverse Proxy (HAProxy) Configuration

HAProxy sits at the network boundary to absorb volumetric traffic and manage network-level rate limits.

- [ ] **Connection & IP Rate Limiting (Stick-Tables)**
  - [ ] Track client connection rates per source IP (`conn_rate(10s)`).
  - [ ] Track HTTP request rates for Smart HTTP (`http_req_rate(10s)`).
  - [ ] Automatically throttle or tarpit abusive clients exceeding baseline cloning rates.

- [ ] **L4 / L7 Denial of Service (DoS) Mitigation**
  - [ ] Enable SYN-flood defense and configure aggressive connection timeouts for idle/stalled handshakes.
  - [ ] Limit maximum concurrent TCP connections per IP address.

- [ ] **TLS Termination (HTTPS)**
  - [ ] Terminate TLS certificates at HAProxy (Let's Encrypt / ACME integration).
  - [ ] Forward clean plaintext HTTP to Evergit on port `:8080` with proper `X-Forwarded-For` and `X-Forwarded-Proto` headers.

- [ ] **SSH Layer 4 TCP Proxying**
  - [ ] Configure HAProxy in `mode tcp` for SSH traffic on port `:2222`.
  - [ ] Pass client source IP where possible (or configure HAProxy PROXY protocol if supported by upstream listener).

---

## 3. Observability & Monitoring

- [ ] Add `/healthz` or `/metrics` HTTP endpoint for health checks and Prometheus scraping.
- [ ] Monitor active conversion count, disk utilization, and SSH session counts.
