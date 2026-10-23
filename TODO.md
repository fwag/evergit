# Evergit Security & Infrastructure Hardening TODO

This checklist tracks security, rate-limiting, and hardening improvements across Evergit and its surrounding edge infrastructure (HAProxy).

---

## 0. Known Vulnerabilities (from security review)

- [x] **[High] Nested repository paths can corrupt existing mirrors**
  - [x] `resolver.ParsePath` accepts `.git` in non-final segments, so `/github/torvalds/linux.git/commondir/x` resolves to `mirrors/github.com/torvalds/linux.git/commondir/x.git`.
  - [x] `syncMirror` runs `os.MkdirAll(filepath.Dir(MirrorPath))` before cloning, creating a `commondir` directory inside the real mirror; git then fails with `failed to read .../commondir: Is a directory`.
  - [x] The broken mirror still passes `looksLikeBareRepo`, so it is never repaired: after the TTL `syncMirror` fails and the repo is unservable until manual cleanup. The request also takes a different repo lock.
  - [x] Fix: reject any non-final segment ending in `.git`, plus a resolver test.

- [ ] **[Medium] Force-pushed history is served publicly**
  - [ ] `refs/evergit-backups/*` reaches the serving repo (`fast-export --all`, and the explicit refspec in `syncNativeSHA256`), so clients can fetch commits that upstream removed, e.g. leaked secrets or DMCA takedowns.
  - [ ] Decide: keep backups in the mirror only and stop serving them, or document the behavior explicitly.

- [x] **[High] Submodule URLs can make the server clone its own local repositories**
  - [x] `resolveSubmoduleInfo` treats any submodule URL starting with `/` or `file://` as a local path, for any parent repository, bypassing `AllowedHosts`. A public repository whose `.gitmodules` points at a server path makes Evergit clone and convert any git repository the daemon can read (disk/CPU use, and a yes/no oracle on local repositories and commits through real vs padded gitlinks).
  - [x] Local mirrors are named by basename only (`mirrors/local/submodule/<name>.git`), so different local repositories collide.
  - [x] A relative `file://` URL is joined onto `https:/github.com/...`, which git parses as an scp-style ssh target with host `https`.
  - [x] Fix: dropped the branch; local paths and non-network URL schemes are rejected (and so need an approval like any submodule that cannot be converted). Tests mirror local upstreams from per-upstream hosts through `UpstreamOverrides`.

- [x] **[Low] Shell injection in `/setup.sh` through the `Host` header**
  - [x] Without `PublicURL`, `r.Host` is rendered unescaped into `HTTP_BASE="..."`; Go accepts `Host: $(id;...)`, which runs in the victim's shell under `curl | bash`. Needs an attacker-influenced Host header (reverse proxy, cache poisoning).
  - [x] Fix: without `-public-url`/`-public-ssh-url` the Host header must be a plain host name or IP address with optional port (400 otherwise); every rendered value must use URL-safe characters (the base URL is also embedded in the installed git aliases), and the assignments use single-quote shell escaping.

- [x] **[Low] Reject `/git-receive-pack` before `EnsureRepo`**
  - [x] Push is currently refused only by `git-http-backend` (no `REMOTE_USER`), after a sync/conversion has already been triggered. Return `403` immediately.

- [x] **[Low] Disable upstream HTTP redirects**
  - [x] git follows the initial redirect (`http.followRedirects=initial`), so a self-hosted allowed host could redirect Evergit to an internal address. Pass `-c http.followRedirects=false` on clone/fetch.

- [x] **[Low] Log injection via SSH command**
  - [x] `executeGitCommand` logs `rawCmd` with `%s`; a client can forge log lines with embedded newlines. Use `%q`.

- [ ] **[Low] `LookupID` scans the whole `loose-object-idx` per request**
  - [ ] Expensive on very large repositories; consider an index or sorted file with binary search.

---

## 1. Evergit In-App Protections (Resource-Aware Controls)

Because HAProxy cannot inspect Git conversion costs or encrypted SSH application state, Evergit must guard its own internal resources.

- [x] **Global JIT Conversion Concurrency Limiter**
  - [x] Implement a global semaphore (e.g., using `golang.org/x/sync/semaphore`) to bound simultaneous `git-fast-export | git-fast-import` pipelines across all repositories.
  - [x] Default concurrency cap to `runtime.NumCPU()` (or expose via `-max-conversions` flag).
  - [x] Queue incoming conversions with timeout rather than spawning unbounded subprocesses.

- [x] **Graceful Load Shedding & Informative Errors**
  - [x] For HTTP: return `HTTP 429 Too Many Requests` or `HTTP 503 Service Unavailable` with `Retry-After` header when conversion queues are saturated.
  - [x] For SSH: write user-friendly rejection messages to `stderr` channel (e.g., `error: server busy, please retry later`) before closing sessions.

- [x] **Disk Space Safeguards & Storage Quotas**
  - [x] Inspect available storage using `syscall.Statfs` before initiating a new upstream mirror clone.
  - [x] Define minimum free disk threshold (e.g., 5 GB or 10% disk capacity).
  - [x] Abort new clones with clear diagnostic logging when capacity threshold is breached.

- [ ] **Cache Maintenance & Pruning**
  - [ ] Implement background LRU or TTL-based garbage collection for stale cached mirrors under `storage/mirrors/` and `storage/builds/`.
  - [ ] Prevent runaway disk consumption from one-off repository clones.

- [x] **Case-Insensitive Path Normalization**
  - [x] `github/Torvalds/Linux` and `github/torvalds/linux` currently produce separate mirrors and conversions, multiplying resource usage and bypassing the per-repo lock.
  - [x] Lowercase owner and repo for hosts known to be case-insensitive (GitHub, GitLab, Bitbucket).

- [x] **SSH Session Limits**
  - [x] Any key is accepted and a session that never sends `exec` is held forever: add an idle timeout for sessions awaiting a request.
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

---

## 4. Submodule Pre-Conversion & Real SHA-256 Translation

- [x] **Opportunistic Submodule Pre-Conversion & Historical Path Resolution**
  - [x] Inspect `HEAD:.gitmodules` and historical commits modifying `.gitmodules` in the mirror before running a repository's conversion pipeline to identify active and historical submodules.
  - [x] Enforce a configurable maximum recursion depth constant (default: `1`, e.g., `DefaultMaxSubmoduleDepth = 1`) to prevent runaway cascading or circular submodule conversions.
  - [x] For submodules hosted on allowed forges (`cfg.AllowedHosts`), pre-convert each submodule repository Just-In-Time via `EnsureRepo`.
  - [x] Load the submodule's translation marks (`evergit-sha1-marks.txt` / `evergit-sha256-marks.txt`) into an in-memory lookup map (`submoduleCommitMap[path][sha1] = sha256`).
  - [x] In `transformFastExportStream`:
    - [x] If `<sha1>` exists in `submoduleCommitMap[path]`, write the **real SHA-256 hash**.
    - [x] If `<sha1>` is not found under `subPath` (e.g. submodule moved or path varied in history), search across all other pre-converted submodules for `<sha1>`.
    - [x] If `<sha1>` is not found in any map (e.g. defunct historical submodules like `.gnulib`), fall back to the synthetic zero-padded hash (`<sha1>` + 24 zeros) so ancient history converts without error.
  - [x] Verify that client `git submodule update --init` successfully fetches and checks out the real SHA-256 commit from Evergit, leaving working tree clean.

---

## 5. Reproducible SHA-256 Conversion

Goal: the same SHA-1 history always converts to the same SHA-256 history, across runs, instances and cache rebuilds.

- [x] **Object-level conversion** following Git's hash function transition design, byte-identical to Git's own `compatObjectFormat` translation (replaces `git fast-export | git fast-import`, which stripped signatures, dropped `mergetag`/unknown headers and tags of trees, normalized modes, forged nested tags and lost files).
- [x] **Consistent submodule gitlinks**
  - [x] Discover submodules from every commit's own `.gitmodules` for the gitlinks it records, instead of `HEAD` plus the last 50 `.gitmodules` changes of `git log --all` (which included instance-specific backup refs, and dropped one of two URLs for the same path at random).
  - [x] Fail the conversion when a submodule has a temporary problem (busy, unreachable, cancelled), never pad; no automatic retry. HTTP 503 with `Retry-After`, SSH message asking to retry.
  - [x] No synthetic SHA-256 without administrator approval: a gitlink without URL, a host outside `-allowed-hosts`, an unusable URL or a repository/commit gone upstream fails the conversion (HTTP 403) with the suggested approval line, until the host is allowed or the submodule is approved in the `-synthetic-gitlinks` file (re-read at every conversion).
  - [x] Refresh a submodule within its TTL when it lacks a recorded commit, and fetch commits that no branch or tag reaches by id (pinned under `refs/evergit-pinned/`).
  - [x] Record every gitlink decision in `evergit-state.json` (by submodule, with reason and approval for synthetic ones) and reuse it in incremental conversions and rebuilds.
  - [x] Convert a repository's submodules the same way however it is reached (the depth limit, now 10, fails instead of padding), so a repository's SHA-256 no longer depends on request order.
  - [x] Submodule cycles fail clearly instead of deadlocking; the visited set is propagated into the conversion job. A submodule pointing into its own repository is translated from its own objects.
  - [ ] Cycles between repositories that are resolvable commit by commit (A@v2 -> B@v1 -> A@v1) still fail; converting them would need cross-repository translation.
- [x] **Stale serving after a failed conversion**
  - [x] When a conversion failed after a successful fetch (e.g. `ErrServerBusy`), the next request saw an unchanged mirror, reported success and served the old version until upstream changed again. "Up to date" is now decided against `converted_refs` in `evergit-state.json`, the mirror's refs recorded as the last step of a successful conversion; natively SHA-256 repositories compare the mirror's refs with the served ones.
- [x] **Conversion versioning**
  - [x] `conversionVersion` recorded in each serving repository's `evergit-state.json` (with the evergit revision, for diagnostics); a repository recorded with another version is rebuilt, keeping synthetic gitlink decisions.
  - [x] Golden test pinning the SHA-256 of a fixed SHA-1 history; a change requires bumping `conversionVersion`.
  - Not needed since object-level conversion: object ids no longer depend on the git version (git only reads objects, indexes and repacks the bytes Evergit writes), so no startup canary, `-git-path` or git version matrix.
- [ ] **Scaling limits**
  - [ ] The SHA-1 -> SHA-256 table is held in memory (~100 bytes per object, ~1 GB for the Linux kernel) and blobs are read whole.
  - [x] Incremental pack merges ran `repack -f` every 8 updates (about 1.5 hours on the Linux kernel) while clients waited. Updates are now merged geometrically; a full `repack -f` runs only once update packs reach 25% of the main pack, while the current copy is served (`-request-wait`).
  - [x] Conversions were cancelled when their clients disconnected and limited to 1 hour, so behind a proxy timeout large repositories never converted. They now run to `-max-conversion-time` (default 12h); requests wait at most `-request-wait`.
  - [ ] Reusing the source pack's blob deltas would avoid recompressing every file version (conversion time is dominated by raw blob volume: 15 min for libvirt's 33 GB).
