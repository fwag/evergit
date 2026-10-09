# Evergit Security & Infrastructure Hardening TODO

This checklist tracks security, rate-limiting, and hardening improvements across Evergit and its surrounding edge infrastructure (HAProxy).

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
