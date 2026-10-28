# Evergit

Evergit is a lightweight, high-performance, resilient Git proxy daemon written in Go. It automatically proxies, caches, and converts Git repositories from legacy **SHA1** to modern **SHA256** object formats on-demand (Just-In-Time). 

It is designed to solve a critical interoperability gap: **Git does not allow referencing SHA1 submodules within a SHA256 superproject**. By proxying and dynamically converting the submodule repository, Evergit allows seamless integration of legacy dependencies into modern, high-security codebases.

---

##  Features

- **Just-In-Time (JIT) Conversion:** Translates SHA1 repositories to SHA256 on the first `git clone` or `git fetch` attempt, object by object as defined by Git's [hash function transition design](https://git-scm.com/docs/hash-function-transition): every object keeps its exact content, including signatures, and only the ids of the objects it references change. The result is identical to what Git's own `compatObjectFormat` translation computes and does not depend on the installed Git version.
- **Robust Local Caching (Upstream Resilience):** Caches bare repositories locally. If the upstream forge (e.g., GitHub, GitLab) is down or has deleted a repository, Evergit continuously serves the last successfully retrieved version from its local cache, insulating your build environment.
- **Force-Push and Deletion Protection (History Archival):** Captures references automatically before fetching updates. If a branch/tag is force-pushed (overwritten) or deleted upstream, Evergit automatically archives the old commits under a dedicated, permanent namespace (`refs/evergit-backups/`). This guarantees that old commits are never lost or garbage collected, and remain fully checkoutable by clients!
- **Fully Compliant Smart HTTP/HTTPS Serving:** Leverages Go's native CGI wrapper over `git-http-backend`, delivering complete out-of-the-box support for the Git Smart HTTP protocol (v1 and v2).
- **Custom SSH Git Daemon:** Employs a secure, lightweight custom SSH server using `golang.org/x/crypto/ssh` that accepts public-key connections, handles directory traversal protection, and directly pipes connection channels into local `git-upload-pack` subprocesses.
- **Flexible Path Resolution:** Resolves short paths like `/github/owner/repo` or `/gitlab/owner/repo` to their corresponding domains, and also supports arbitrary nested paths for GitLab subgroups (e.g., `/gitlab.com/org/subgroup1/subgroup2/repo`) and hosts serving repositories at the top level (e.g., `/boringssl.googlesource.com/boringssl`). Only upstream hosts on the allowlist are proxied (see `-allowed-hosts`), so clients cannot point Evergit at internal services.

### Limitations

- **Only branches and tags are mirrored.** Forge-specific refs such as GitHub's `refs/pull/*` or GitLab's `refs/merge-requests/*` are not served.
- **Submodules must be convertible, or explicitly approved.** Evergit converts the submodules a history records and uses their **real SHA-256 commit hashes**. When a submodule cannot be converted (no URL in the recording commit's `.gitmodules`, a host outside `-allowed-hosts`, an unusable URL, or a repository or commit that no longer exists upstream), the conversion fails with a message naming the submodule, until the administrator allows its host or approves recording its commits with a synthetic SHA-256 (`<sha1>` + 24 zeros) in the `-synthetic-gitlinks` file. A temporarily unavailable submodule also fails the conversion, which can be retried. So the result never depends on when or where it ran.

---

##  Installation & Setup

Ensure you have **Go 1.18+** and **Git 2.42.0+** installed on your system.

### 1. Build the Binary
```bash
go build -o evergit ./cmd/evergit
```

### 2. Run the Daemon
```bash
./evergit -http :8080 -ssh :2222 -storage ./storage -ttl 30s
```

### Command-Line Flags
- `-http`: HTTP listen address (default: `:8080`)
- `-ssh`: SSH listen address (default: `:2222`)
- `-public-url`: Public base URL of this Evergit HTTP service (e.g. `http://evergit.local:8080`); also settable via `EVERGIT_PUBLIC_URL`. Recommended, together with `-public-ssh-url`: without them the client setup script derives its URLs from the request's `Host` header, which must then be a plain host name or IP address
- `-public-ssh-url`: Public base URL of this Evergit SSH service (e.g. `ssh://git@evergit.local:2222`); also settable via `EVERGIT_PUBLIC_SSH_URL`
- `-storage`: Directory path for storing host keys, cache mirrors, and converted repositories (default: `./storage`)
- `-ttl`: Cache duration before checking upstream for updates; also settable via `EVERGIT_CACHE_TTL` (default: `30s`, pass `0` to check upstream on every fetch)
- `-max-conversions`: Maximum concurrent JIT conversions; also settable via `EVERGIT_MAX_CONVERSIONS` (default: CPU count)
- `-request-wait`: How long a request waits for a running sync or conversion; also settable via `EVERGIT_REQUEST_WAIT` (default: `20s`, `0` waits until the end). After it, an existing copy is served (it stays consistent while being updated) and a first conversion is answered with HTTP 503 and `Retry-After`; over SSH a first conversion keeps the client waiting with progress. Conversions always continue when clients disconnect. Keep it below the reverse proxy's timeout.
- `-max-conversion-time`: Maximum duration of a single sync or conversion; also settable via `EVERGIT_MAX_CONVERSION_TIME` (default: `12h`; a first conversion of the Linux kernel takes about 4 hours on 4 CPUs).
- `-max-submodule-depth`: Maximum nesting of submodules converted for a repository; also settable via `EVERGIT_MAX_SUBMODULE_DEPTH` (default: `50`). It only stops runaway nesting: exceeding it fails the conversion. Repositories recording each other as submodules count a level for each translation (the forks of mbedtls reached from qemu through edk2 need 24).
- `-min-free-disk`: Minimum free disk in GB required before cloning new mirrors; also settable via `EVERGIT_MIN_FREE_DISK_GB` (default: `1`)
- `-debug`: Enable debug logging; also settable via `EVERGIT_DEBUG` (default: `false`)
- `-allowed-hosts`: Comma-separated upstream hosts clients may request; also settable via `EVERGIT_ALLOWED_HOSTS` (default: `github.com,gitlab.com,bitbucket.org`). Self-hosted forges must be added explicitly. Upstream redirects are only followed over HTTPS to allowed hosts (git itself never follows them), so a host that redirects elsewhere needs its target allowed too, e.g. `git.savannah.gnu.org,https.git.savannah.gnu.org`.
- `-synthetic-gitlinks`: File approving synthetic SHA-256s for submodules that cannot be converted (see [Submodules that cannot be converted](#submodules-that-cannot-be-converted)); also settable via `EVERGIT_SYNTHETIC_GITLINKS`. It is read again at every conversion, so approvals apply without a restart.
- `-shorthands`: Comma-separated domain shorthand mappings in `alias=domain` format; also settable via `EVERGIT_SHORTHANDS` (default: `github=github.com,gitlab=gitlab.com,bitbucket=bitbucket.org`). Pass `none` to disable shorthands.

---

##  Usage (Cloning and Submodules)

### Client Auto-Configuration (Recommended)

Evergit provides a client setup script that configures Git to route through Evergit **without polluting or breaking your global git configuration**.

By default, the script writes its proxy rules to an isolated file (`~/.config/evergit/gitconfig`) and uses Git's native `includeIf` feature to activate it **only for repositories inside a specific directory** (e.g. `~/tmp/`, `~/obs/`, or your current workspace):

```bash
# Scope Evergit to your current directory (recommended)
curl -fsSL http://localhost:8080/setup.sh | bash

# Or scope to a specific build workspace (e.g. ~/tmp or ~/obs)
curl -fsSL http://localhost:8080/setup.sh | bash -s -- --dir ~/tmp
```

#### Why Directory Scoping?
* **Inside the target directory** (e.g. `~/tmp/libvirt` and its submodules): All clones and submodule checkouts automatically route through Evergit and convert to SHA-256.
* **Outside the target directory** (e.g. your daily development in other folders): Git connects directly to `github.com` and `gitlab.com` normally. Pushes, private repositories, and direct fetches remain 100% unaffected.

**Options:**
```bash
# Scope to a specific directory
curl -fsSL http://localhost:8080/setup.sh | bash -s -- --dir ~/build

# Configure default remote name (e.g. upstream for fork/subtree repos)
curl -fsSL http://localhost:8080/setup.sh | bash -s -- --dir ~/build --remote upstream

# Configure for SSH clones instead of HTTP
curl -fsSL http://localhost:8080/setup.sh | bash -s -- --dir ~/tmp --ssh

# Preview commands without applying them
curl -fsSL http://localhost:8080/setup.sh | bash -s -- --dry-run

# Revert and remove Evergit rules from ~/.gitconfig
curl -fsSL http://localhost:8080/setup.sh | bash -s -- --uninstall

# Apply globally across the entire machine (intrusive)
curl -fsSL http://localhost:8080/setup.sh | bash -s -- --global
```

---

### Manual Usage

Evergit automatically resolves requests by parsing the URL path segments.

### 1. Cloning via HTTP/HTTPS
To clone a repository from GitHub through Evergit:
```bash
git clone http://localhost:8080/github.com/owner/repo.git
```
*(Or use shorthand format: `http://localhost:8080/github/owner/repo.git`)*

### 2. Cloning via SSH
To clone using the custom SSH daemon:
```bash
git clone ssh://git@localhost:2222/github.com/owner/repo.git
```
*(Or shorthand format: `ssh://git@localhost:2222/github/owner/repo.git`)*

### 3. Adding as a Submodule in a SHA256 Project
To add a legacy SHA1 repository as a SHA256 submodule in your modern superproject:
```bash
# Go to your SHA256 superproject directory
git submodule add http://localhost:8080/github.com/owner/legacy-sha1-repo.git path/to/submodule
```
Evergit will dynamically present the SHA256-converted version of the repository to your superproject, which can then commit and trace the submodule hash correctly!

### 4. Working with Legacy SHA-1 Hashes on the Client
Evergit keeps the SHA-1 ↔ SHA-256 correspondence of every converted commit and answers lookups over HTTP:

```bash
curl http://localhost:8080/github.com/owner/repo.git/sha1/722a661     # SHA-1 (full or ≥7 chars) -> SHA-256
curl http://localhost:8080/github.com/owner/repo.git/sha256/<sha256>  # SHA-256 (full or ≥7 chars) -> SHA-1
```

For convenience, configure Git aliases (they need `curl`). You can include the repository's shared `.gitconfig`:

```bash
git config --local include.path ../.gitconfig
```

*(Alternatively, you can configure them globally via `git config --global alias.from-sha1 ...`, `alias.to-sha1 ...`, and `alias.update-submodules ...`)*

Then, inside a clone of a converted repository (e.g. a submodule or subtree):
```bash
git checkout $(git from-sha1 722a661)            # check out a legacy SHA-1 commit
git checkout $(git from-sha1 722a661 upstream)   # ...or specify remote explicitly
git to-sha1                                      # legacy SHA-1 id of HEAD
git to-sha1 v1.2.0                               # ...or of any revision
git to-sha1 HEAD upstream                        # ...or with explicit remote
```

#### Remote Resolution & Subtree / Fork Support
The aliases resolve the Evergit repository URL using the following priority:
1. **Explicit argument:** Pass the remote name directly (e.g. `git from-sha1 722a661 upstream`).
2. **Repository configuration:** Pin the remote per repository via `git config evergit.remote upstream`.
3. **Auto-detection:** Automatically finds any configured remote pointing to Evergit, or checks `upstream` before `origin` (ideal for fork and subtree repositories where `origin` points to an internal forge).
4. **Explicit Evergit URL:** For clones made over SSH without matching remotes, set `evergit.url` once per clone (or in `.gitconfig`):
```bash
git config evergit.url http://localhost:8080/github.com/owner/repo.git
```

### Working with Submodules

When using Evergit's client setup script (`curl .../setup.sh | bash`), submodules work natively with standard Git commands:
```bash
git submodule update --init
```

#### How it works under the hood:
1. **Discovery from the history itself:**
   Before converting a superproject (e.g. `libvirt`), Evergit lists every submodule commit its history records, with the URL that *the recording commit's own* `.gitmodules` gives for the path, including submodules that were removed or moved long ago.
2. **Conversion of the submodules:**
   Each submodule (e.g. `keycodemapdb`) is converted the same way, recursively, so its SHA-256 is the same whether it is cloned directly or reached as a submodule. If it lacks a recorded commit, it is refreshed regardless of the cache TTL, and commits that no branch or tag reaches (e.g. only a pull request ref) are fetched by id and kept under `refs/evergit-pinned/`.
3. **Real Cryptographic SHA-256 Translation:**
   Gitlinks record the submodule's real SHA-256 commit, from its `objects/loose-object-idx`. A submodule pointing into its own repository (e.g. a `gh-pages` docs submodule) is translated from that repository's objects. Every decision is recorded, by submodule and with the reason and approval for synthetic ones, in the serving repository's `evergit-state.json` (never served to clients), so later updates and rebuilds never translate a submodule commit differently.
4. **Failures instead of guesses:**
   A temporary failure (busy server, unreachable upstream) fails the conversion with a message asking to retry (HTTP 503 with `Retry-After`). A submodule that cannot be converted for good fails it until the administrator decides (HTTP 403, see below). Submodule cycles between repositories and nesting deeper than 10 levels fail with an error.
5. **Seamless Client Clones:**
   When your client runs `git submodule update --init`, the request routes to Evergit via the scoped `insteadOf` rule. The client requests the real SHA-256 commit from Evergit, which Evergit serves directly from its cache, resulting in a clean working tree.

#### Submodules that cannot be converted

Some submodules cannot be converted: the recording commit's `.gitmodules` gives no URL, the host is not in `-allowed-hosts`, the URL is unusable (e.g. `git://`), or the repository or commit no longer exists upstream. Cloning then fails with a message for the administrator, for example:

```
remote: Evergit: submodule needs administrator approval: submodules of gitlab.com/libvirt/libvirt cannot be converted:
remote:   https://git.savannah.gnu.org/git/gnulib.git/: upstream host "git.savannah.gnu.org" is not allowed (10 commit(s))
remote:
remote: Ask the Evergit administrator to allow their hosts (-allowed-hosts), or to approve synthetic SHA-256s for them in the synthetic gitlinks file (-synthetic-gitlinks):
remote:   gitlab.com/libvirt/libvirt https://git.savannah.gnu.org/git/gnulib.git/
```

The administrator either adds the host to `-allowed-hosts`, so the submodule is converted, or approves recording its commits with a synthetic SHA-256 (`<sha1>` + 24 zeros) by adding the suggested line to the `-synthetic-gitlinks` file:

```
# <superproject>              <submodule: URL as in .gitmodules, or path:<path> for a gitlink without URL>
gitlab.com/libvirt/libvirt    git://git.sv.gnu.org/gnulib.git
gitlab.com/libvirt/libvirt    path:.gnulib
*                             https://git.savannah.gnu.org/git/gnulib.git/
```

`*` approves a submodule for every superproject. Once recorded, a decision is kept for the repository, also if the approval is later removed.

> **Do not enable `extensions.compatObjectFormat` in your clones or in the superproject.** Git's SHA-1 compatibility mode
> must compute a SHA-1 id for every object it writes, and it cannot do so for objects received by `git fetch` or for
> submodule commits: with it enabled, `git fetch` / `git submodule update` and commits that pin a submodule fail with
> `Failed to convert object from sha256 to sha1`. Use the lookups above instead; clones stay plain SHA-256 repositories.

---

##  Security Architecture

1. **OS Command Injection Prevention:** Evergit completely avoids shell execution (`/bin/sh -c`). The custom SSH server parses incoming connection payloads structurally and invokes `os/exec.Command` directly with explicit arguments, and only `git-upload-pack` is allowed.
2. **Path Validation:** Every repository path segment is restricted to `[A-Za-z0-9._-]` (no `.`/`..` or empty segments), and the resolved paths are verified to stay within the storage root, for both HTTP and SSH.
3. **Upstream Allowlist:** Only hosts listed in `-allowed-hosts` are proxied, so clients cannot make Evergit reach internal services or arbitrary ports.
4. **Isolated Git Environment:** Inherited variables that would redirect git to another repository (`GIT_DIR`, `GIT_OBJECT_DIRECTORY`, ...) are stripped from every git invocation.
5. **Atomic Publishing:** Full conversions are built separately and published with an atomic symlink swap, so clients never observe a missing or half-written repository.
6. **Bounded Operations:** HTTP header reads, SSH handshakes and upstream transfers have timeouts; stalled upstream transfers are aborted, and a slow client cannot stall a conversion.
7. **Concurrency Locking:** Syncs and conversions of the same repository are serialized with per-repository mutexes, and an exclusive lock on the storage root prevents two Evergit instances from sharing it.

### Deployment Recommendations

- **Serve over HTTPS.** Evergit speaks plain HTTP. Put it behind a TLS-terminating reverse proxy: the SHA-1 ↔ SHA-256 lookups and `loose-object-idx` are trusted by clients to translate legacy ids, and a tampered answer would point them at a different commit.
- **Rate-limit and authenticate at the proxy.** Evergit has no authentication, quotas or rate limits; every request for a new repository triggers an upstream clone and a full conversion. The SSH server accepts any public key, as it is intended for anonymous read-only access.
- **Archived history is kept forever.** Backup refs under `refs/evergit-backups/` are never expired; repositories with frequent force-pushes grow accordingly.

---

##  Testing

The test suite runs against real `git` binaries: unit tests for path resolution, conversion, force-push archival, the compatibility map and failure recovery, plus end-to-end tests that start ephemeral HTTP/SSH servers and clone from them. Tests isolate git from your global configuration.

```bash
go test -v ./...
```
