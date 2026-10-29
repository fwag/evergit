# Converting submodules: plan, then execute

Status: implemented (`plan.go`, `execute.go`). The section *As implemented* lists where the
implementation differs from the proposal.

## Problem

Converting a repository requires the SHA-256 of every submodule commit its history records, so its
submodules are converted first, recursively. Today this is a mutual recursion through five functions
and a goroutine:

```
resolveGitlinks(A)
 ├─ convertSubmodule(B) → ensureSubmoduleCommits → ensureRepo → [job] → doEnsureRepo → resolveGitlinks(B) → …
 └─ convertSubmodule(B) → translateCommits(B)                                         → resolveGitlinks(B) → …
                          (B further up the chain, or A itself)
```

Consequences:

- Two code paths translate submodule commits: a persisted partial conversion, and an in-memory one
  for a repository whose conversion is waiting further up the chain. The in-memory one caused the
  bug fixed in 519e181 (translations not used as gitlink decisions).
- Every level of either loop counts against the depth limit: the forks of mbedtls reached from qemu
  need 24.
- `resolveGitlinks` has a cognitive complexity of 138 (`golangci-lint run --new-from-rev=`), mostly
  closures capturing the state of the recursion.
- A conversion stops at the first submodule needing an approval: for qemu, the approvals were found
  one per run of about 30 minutes.

## Proposal

Split a conversion into two phases.

### 1. Plan

Breadth-first over items `(repository, commits)`, starting with the requested repository and its refs
(or the required commits of a partial conversion). Nothing is converted. For each item:

1. Sync the repository's mirror, once per plan, and make the commits available (`fetchCommits`, with
   pins for submodules).
2. Scan the gitlinks recorded in the history of the commits, excluding history converted before or
   already scanned in this plan.
3. For each gitlink `(URL, commit)`:
   - URL of a repository: add the commit to that repository's item, queueing it if new.
   - Otherwise (no URL, unusable URL, commit gone): a *problem*, resolved after the scan.
4. Resolve problems with the rules of today, all deciding from the history only: another URL recording
   the same commit, another URL of the same path (aliases), the repository itself, then the approved
   synthetic SHA-256. A candidate is only used if its upstream has the commit (`fetchCommits`); it then
   becomes an item like any other.

At the end, all missing approvals of the whole plan are reported at once, before converting anything.

The result is, for every repository, the set of commits to convert and, for every commit recorded as
a gitlink, where its SHA-256 comes from.

### 2. Execute

Convert items in dependency order, each as a partial, incremental conversion (adcddd9). An item is
ready when every gitlink in the history it converts is decided: recorded before, synthetic, or
translated by an item converted earlier.

- Repositories are ordered topologically on the graph "records a commit of". Most plans are acyclic.
- A cycle (strongly connected component) is executed commit by commit: a commit can only record
  commits that existed before it, so some commit of the cycle is always ready. For the mbedtls forks:

  ```
  TF-PSA-Crypto{a1} → mbedtls-framework{b2} → TF-PSA-Crypto{a3} → …
  ```

  instead of TF-PSA-Crypto waiting for mbedtls-framework waiting for TF-PSA-Crypto.
- The requested repository is the last item, converted completely if it was requested itself.

## Invariants

- **Decisions depend only on history.** Where a gitlink's SHA-256 comes from is decided from the
  repositories' histories, the upstreams' answers (commit gone or not) and the administrator's
  configuration (approvals, allowed hosts), never from the order of execution or what other requests
  converted before. Execution only computes object ids, which depend on the objects alone.
- **Nothing converted changes.** Items only extend repositories (partial or incremental conversions),
  and recorded decisions are kept, as today.
- **Every step is idempotent.** Converting commits already converted adds nothing, so an interrupted
  plan resumes, and concurrent plans sharing repositories do not conflict.

## Locking

Today a conversion holds its repository's job for its whole duration, and recursion through
repositories further up the chain is what `beingConverted` and `translateCommits` work around. With a
plan, a job runs plan and execution in the background (it still outlives the request, as today), and
each execution step takes the lock of its own repository only for that step. No step waits on another
plan while holding a lock, so concurrent plans with overlapping repositories cannot deadlock; they
serialize per repository, and a step finding its commits converted does nothing.

## What goes away

- `translateCommits` and the in-memory translations: every translation is a persisted partial
  conversion.
- `beingConverted`, `withVisitedRepo` and `ErrSubmoduleCycle`: cycles are ordinary.
- The recursion and the depth limit: `-max-submodule-depth` becomes a limit on the size of a plan
  (repositories, or levels of the breadth-first walk), still only stopping runaway plans.
- `resolveGitlinks` as it is: its parts become the plan's scan and problem resolution, as methods of a
  plan type instead of closures.

## Implementation steps

Each step keeps all tests passing.

1. **Plan alongside the current code.** `plan.go` with the plan type and the breadth-first walk,
   tested on its own (items and problems for cycles, forks, aliases, gone commits). Not used yet.
2. **Report all approvals at once.** Run the plan before converting, only to fail early with the
   complete list of missing approvals. Conversion still recursive.
3. **Execute from the plan.** Replace the recursive conversion of submodules by the execution of the
   plan, cycles included. `translateCommits`, `beingConverted` and the recursion are removed.
4. **Plan size limit** replacing the depth limit (flag and configuration kept compatible, or renamed).

Verification at the end, as for the current branch: qemu from scratch, the same SHA-256 for the
converted commits as `submodule-cycles` (same decisions), and fewer runs to collect approvals.

## Open questions

- **Plan size limit:** number of repositories, number of commits, or levels? A level limit is closest
  to today's flag; a repository limit is easier to reason about for an administrator.
- **Mirror sync for submodules:** a mirror is fetched only when required commits are missing, as now.
  Should a plan also refresh submodule mirrors whose commits are all present, to see new aliases or
  refs? Today: no, as commits never change; aliases come from the superproject's history.
- **Progress reporting:** the plan gives totals (repositories, commits) before converting; worth
  showing to clients as progress, e.g. `remote: Evergit: plan: 107 repositories, 3 cycles`.
- **Partial items for the requested repository:** a superproject requested itself is converted
  completely; a plan started for a submodule (e.g. from `git submodule update`) completes that
  repository too (A.a). No change intended.

## As implemented

- **Execution follows the gitlinks, not a precomputed order.** Converting `(repository, commits)`
  first converts, in the repositories the plan decided, the commits recorded in their history, then
  the repository: one small recursive function over the plan (`conversionPlan.convert`), guarded
  against re-entering the same step. A cycle thus advances commit by commit without computing
  strongly connected components.
- **Three kinds of steps:** a complete conversion (the requested repository, or a repository
  converted completely with other rules, rebuilt), a partial conversion pinning the step's commits,
  and for a repository converted completely an *extension*, which adds the commits and serves them
  as pinned refs without changing its branches, tags or TTL. Either way the step's commits are
  served, so `git submodule update` finds them even over SSH, where git uses protocol v0, which only
  fetches advertised commits. The plan fetches commits without pinning them; a step pins the commits
  it converts.
- **Sources instead of tables.** A conversion no longer searches every submodule's translation table:
  each gitlink's SHA-256 comes from the source the plan decided, so `gitlinkResolution` only holds
  decisions.
- **Limit:** `-max-submodule-repos` (default 500) counts the submodule repositories of a plan,
  replacing `-max-submodule-depth`; 0 allows none, as depth 0 did.
- **Progress:** clients see the scans, `Resolving N submodule(s)...`, and `Plan: N repositories, M to
  convert` before the conversions.
