# Low-Level Design (LLD): Evergit Execution Engine (`execute.go`)

This technical document specifies the Low-Level Design for `internal/converter/execute.go`. All text complies with **ASD-STE100 (Simplified Technical English)** rules.

---

## 1. Scope and Architectural Role

The `execute.go` module executes a pre-computed `conversionPlan`. It transforms repositories from SHA-1 to SHA-256 in strict topological dependency order.

### Core Invariants

* **Dependency Post-Order:** A repository converts only after all repositories providing its gitlink translations convert their required commits.
* **Time-Directed Acyclicity:** Gitlinks point only to past commits. Therefore, commit history always forms a Directed Acyclic Graph (DAG) across time, even if repositories record each other.
* **Dynamic Cycle Detection:** The execution stack tracks the active repository and heads. If recursive execution encounters the same state, execution stops immediately with `ErrSubmoduleUnavailable`.
* **No Unplanned Gitlinks:** Every gitlink encountered in mirror history during conversion must exist in the planned sources table.
* **Translation Consistency:** One SHA-1 submodule commit must map to exactly one SHA-256 hash. Conflicting translations cause an immediate fatal error.
* **Protocol v0 Reachability:** Commits converted for superprojects must be pinned under `refs/evergit-pinned/<sha1>` so that Git clients using SSH protocol v0 can fetch them during `git submodule update`.
* **Mutual Exclusion:** Concurrent operations on the same repository are serialized with `m.repoMutex(r.info)`.

---

## 2. Static Call Tree

```text
p.convert(r, commits)
  │
  ├── [Memoization Check: if complete && p.completed[r] -> return nil]
  ├── [Incremental Check: heads = p.untranslated(r, commits)]
  ├── [Cycle Detection: key = r.id + " " + heads; check p.converting[key]]
  │
  ├── p.needs(r, heads, complete)
  │     ├── [if complete]: r.planned()
  │     │     └── iterates r.sources and filters out r.previouslyDecided
  │     └── [if partial]:  p.stepGitlinks(r, heads)
  │           ├── discoverGitlinks(ctx, MirrorPath, heads, convertedRefs(r), nil)
  │           │     ├── scanGitlinks() -> git log --raw
  │           │     └── gitmodulesURLs() -> git cat-file
  │           └── r.planned()
  │                 ├── matches discovered targets against planned sources
  │                 └── validates invariant: no unplanned gitlink exists
  │
  ├── [Recursive Dependency Invocation]
  │     └── for id in sortedKeys(needs):
  │           └── p.convert(p.items[id], needs[id])
  │
  ├── p.step(r, heads, complete)
  │     ├── mu := p.m.repoMutex(r.info); mu.Lock()
  │     │
  │     ├── p.resolution(r)
  │     │     ├── loadState(ServingPath) -> previous.gitlinkDecisions()
  │     │     ├── newGitlinkResolution(decided)
  │     │     ├── r.planned()
  │     │     └── for each plannedGitlink:
  │     │           ├── p.value(l.src, l.id, tables)
  │     │           │     ├── [synthetic]: syntheticID(id)
  │     │           │     └── [repo]: openTranslationIndex(sha1IndexFile) -> lookup()
  │     │           └── res.add(l, newID) -> validates no conflicting SHA-256
  │     │
  │     ├── [Pinning and Fetching]
  │     │     └── p.m.fetchCommits(ctx, r.info, pins, true)
  │     │           └── pins commits under refs/evergit-pinned/<sha1>
  │     │
  │     └── [Object Rewriting]
  │           └── p.m.convertRepo(ctx, r.info, progressWriter, res, scope)
  │                 └── convertObjects() -> rewriteObjects() -> writeTranslationIndexes()
  │
  └── [Mark Completed: if complete -> p.completed[r] = true]
```

---

## 3. Time Invariance and Mutual References

A repository graph can contain mutual references. For example, Repository A references Repository B, and Repository B references Repository A. Evergit executes without cycles because Git commits exist in strict chronological order.

### The Time-Directed Principle

A gitlink records only a commit that existed *before* the commit that records it. Therefore, following gitlinks backward in history always goes backward in time. Converting one commit of a mutual reference requires only *older* commits of the other repository, never itself.

```text
Repository A:   [A@C1 Base] <────────┐ (gitlink)
                     ▲               │
                     │ (gitlink)     │
Repository B:        └────────── [B@C2 Records A@C1]
```

The execution engine converts commits in chronological dependency order:
1. `A@C1` converts first.
2. `B@C2` converts second using the rewritten SHA-256 of `A@C1`.
3. If `A@C3` later references `B@C2`, `A@C3` converts third.

---

## 4. Data Structures and Schemas

### `plannedGitlink`
```go
type plannedGitlink struct {
    submodule, commit string
    id                sha1ID
    src               gitlinkSource
}
```
* `submodule`: Target submodule URL or `path:<path>`.
* `commit`: SHA-1 hexadecimal hash string.
* `id`: 20-byte binary hash.
* `src`: Pointer to target `*planItem`, synthetic rule, or own repository.

### `conversionPlan` Execution Maps
* `completed map[*planItem]bool`: Memoizes completed repositories. Prevents duplicate conversion.
* `converting map[string]bool`: Stack set storing `"repoID head1 head2..."` for dynamic cycle detection.

### `gitlinkResolution`
* `decided map[sha1ID]sha256ID`: Final mapping of SHA-1 commits to rewritten SHA-256 commits.
* `recorded map[string][]string`: Submodule commit hashes recorded per submodule URL.
* `synthetic map[string]string`: Approval reasons recorded per synthetic submodule URL.

---

## 5. Algorithmic Procedures

### 5.1 `convert(r, commits)`
1. Set `complete = (commits == nil || r.rebuild)`.
2. If `complete == true` and `p.completed[r] == true`, return nil immediately.
3. If `complete == false`, call `untranslated(r, commits)` to obtain `heads`. If none, return nil.
4. Construct key: `key = r.id + " " + strings.Join(heads, " ")`.
5. If `p.converting[key] == true`, halt with `ErrSubmoduleUnavailable` (cycle detected).
6. Set `p.converting[key] = true` with deferred deletion.
7. Call `needs(r, heads, complete)` to identify required submodule commits.
8. For each submodule in alphabetical order, call `convert(p.items[id], needs[id])`.
9. Call `step(r, heads, complete)` to rewrite objects.
10. If `complete == true`, set `p.completed[r] = true`. Return nil.

### 5.2 `needs(r, heads, complete)` & `stepGitlinks(r, heads)`
1. If `complete == true`, get all planned gitlinks with `r.planned()`.
2. If `complete == false`, discover gitlinks in `heads` using `discoverGitlinks()`.
3. Filter `r.planned()` against discovered gitlinks. Verify all discovered gitlinks were planned.
4. Build `needs` map: for each planned link with `l.src.repo != nil` (excluding own repo on complete conversion), append `l.commit` to `needs[repo.id]`.
5. Return `needs`.

### 5.3 `step(r, heads, complete)`
1. Acquire mutex: `p.m.repoMutex(r.info).Lock()`.
2. Build resolution map: `res, err := p.resolution(r)`.
3. Determine pins: `pins = r.required` if complete, else `pins = heads`.
4. If `len(pins) > 0`, call `p.m.fetchCommits()` to ensure commits exist and pin them under `refs/evergit-pinned/<sha1>`.
5. Call `p.m.convertRepo()` to rewrite objects from SHA-1 to SHA-256. Return nil.

### 5.4 `resolution(r)` & `value(src, id, tables)`
1. Load previously recorded decisions with `loadState()`.
2. For each planned gitlink `l`:
   * If `src.synthetic != nil`, return `syntheticID(id)`.
   * If `src.repo != nil`, open or reuse `loose-object-idx` from `src.repo.info.ServingPath` and look up `id`.
   * If found, call `res.add(l, newID)`.
3. Return `res`.

### 5.5 `add(l, newID)`
1. If `l.id` was already decided, verify `previous == newID`. If different, halt with fatal error.
2. Set `g.decided[l.id] = newID`.
3. Append `l.commit` to `g.recorded[l.submodule]`.
4. Return nil.

---

## 6. Error Handling Matrix

| Condition | Detection Point | Safety Action | Final Error |
| :--- | :--- | :--- | :--- |
| Dynamic dependency cycle | `convert()`: `converting[key]` | Stops recursion stack | `ErrSubmoduleUnavailable: converting <key> requires converting itself` |
| Gitlink not planned | `stepGitlinks()` | Stops before Git object mutation | `"gitlink <commit> of submodule <url> was not planned"` |
| Conflicting SHA-256 hash | `res.add()` | Prevents hash divergence | `"submodule commit <c> has conflicting SHA-256 translations"` |
| Pinned commit missing | `step()`: `fetchCommits()` | Fails before rewriting starts | `"<repoID>: <network/fetch error>"` |
| Translation table error | `value()` | Skips unresolved commit | Fails downstream in `convertObjects` if needed |
