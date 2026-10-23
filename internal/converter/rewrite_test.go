package converter

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRaw runs git and returns its untrimmed stdout, optionally feeding stdin.
func gitRaw(t *testing.T, dir string, stdin []byte, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v failed: %v (stderr: %q)", args, err, stderr.String())
	}
	return out
}

func gitLine(t *testing.T, dir string, stdin []byte, args ...string) string {
	t.Helper()
	return strings.TrimSpace(string(gitRaw(t, dir, stdin, args...)))
}

// newCompatRepo creates a SHA-1 repository in which git itself computes the SHA-256 translation of
// every object written, serving as the reference implementation of the transition design.
func newCompatRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "compat")
	gitRaw(t, "", nil, "init", "-q", "--bare", "--object-format=sha1", repo)
	gitRaw(t, repo, nil, "config", "core.repositoryformatversion", "1")
	gitRaw(t, repo, nil, "config", "extensions.compatObjectFormat", "sha256")
	cmd := exec.Command("git", "hash-object", "-w", "--stdin")
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader("probe")
	if err := cmd.Run(); err != nil {
		t.Skipf("git does not support extensions.compatObjectFormat: %v", err)
	}
	return repo
}

func compatSHA256(t *testing.T, repo, id string) string {
	t.Helper()
	return gitLine(t, repo, nil, "rev-parse", "--output-object-format=sha256", id)
}

func TestTranslationMatchesGitCompatObjectFormat(t *testing.T) {
	repo := newCompatRepo(t)
	write := func(typ, content string) string {
		return gitLine(t, repo, []byte(content), "hash-object", "-t", typ, "-w", "--literally", "--stdin")
	}

	blob := write("blob", "hello\n")
	link := write("blob", "target")
	subtree := write("tree", "100644 a\x00"+string(mustHex(t, blob)))
	tree := write("tree", "100664 odd-mode\x00"+string(mustHex(t, blob))+
		"120000 link\x00"+string(mustHex(t, link))+
		"40000 sub\x00"+string(mustHex(t, subtree)))
	ident := "T <t@e> 1700000000 +0000"
	root := write("commit", "tree "+tree+"\nauthor "+ident+"\ncommitter "+ident+"\n\nroot\n")
	side := write("commit", "tree "+subtree+"\nparent "+root+"\nauthor "+ident+"\ncommitter "+ident+"\n\nside\n")
	pgp := "-----BEGIN PGP SIGNATURE-----\n\niQEzBAABCAAdFiEE\n=abcd\n-----END PGP SIGNATURE-----\n"
	ssh := "-----BEGIN SSH SIGNATURE-----\nU1NIU0lH\n-----END SSH SIGNATURE-----\n"
	folded := func(header, sig string) string {
		return header + " " + strings.ReplaceAll(strings.TrimSuffix(sig, "\n"), "\n", "\n ") + "\n"
	}
	tagHeader := "type commit\ntag v1\ntagger " + ident + "\n"
	signedTag := "object " + side + "\n" + tagHeader + "\nrelease\n" + pgp

	cases := []struct{ name, typ, content string }{
		{"tree with odd mode, symlink and subtree", "tree", "100664 odd-mode\x00" + string(mustHex(t, blob)) +
			"120000 link\x00" + string(mustHex(t, link)) + "40000 sub\x00" + string(mustHex(t, subtree))},
		{"plain commit", "commit", "tree " + tree + "\nparent " + root + "\nauthor " + ident + "\ncommitter " + ident + "\n\nmsg\n"},
		{"commit without trailing newline", "commit", "tree " + tree + "\nauthor " + ident + "\ncommitter " + ident + "\n\nno newline"},
		{"commit with encoding", "commit", "tree " + tree + "\nauthor " + ident + "\ncommitter " + ident + "\nencoding ISO-8859-1\n\ncaf\xe9\n"},
		{"signed commit", "commit", "tree " + tree + "\nparent " + root + "\nauthor " + ident + "\ncommitter " + ident + "\n" + folded("gpgsig", pgp) + "\nsigned\n"},
		{"dual-signed commit", "commit", "tree " + tree + "\nauthor " + ident + "\ncommitter " + ident + "\n" + folded("gpgsig", pgp) + folded("gpgsig-sha256", ssh) + "\ndual\n"},
		{"merge with signed mergetag", "commit", "tree " + tree + "\nparent " + root + "\nparent " + side + "\nauthor " + ident + "\ncommitter " + ident + "\n" +
			folded("mergetag", signedTag) + "\nmerge\n"},
		{"annotated tag", "tag", "object " + side + "\n" + tagHeader + "\nrelease\n"},
		{"PGP-signed tag", "tag", signedTag},
		{"SSH-signed tag", "tag", "object " + side + "\n" + tagHeader + "\nrelease\n" + ssh},
		{"tag of tree", "tag", "object " + tree + "\ntype tree\ntag t\ntagger " + ident + "\n\ntree tag\n"},
		{"tag with SHA-256 header signature", "tag", "object " + side + "\n" + tagHeader + folded("gpgsig-sha256", ssh) + "\nrelease\n" + pgp},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := write(tc.typ, tc.content)
			want := gitRaw(t, repo, nil, "cat-file", tc.typ, compatSHA256(t, repo, id))

			mapHex := func(s string) (string, error) { return compatSHA256(t, repo, s), nil }
			var got []byte
			var err error
			switch tc.typ {
			case "tree":
				got, err = translateTree([]byte(tc.content), func(_, _ []byte, id sha1ID) (sha256ID, error) {
					var newID sha256ID
					copy(newID[:], mustHex(t, compatSHA256(t, repo, hex.EncodeToString(id[:]))))
					return newID, nil
				})
			case "commit":
				got, err = translateCommit([]byte(tc.content), mapHex)
			case "tag":
				got, err = translateTag([]byte(tc.content), mapHex)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("translation differs from git\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTranslateCommitKeepsUnknownHeaders(t *testing.T) {
	sha1 := strings.Repeat("1", 40)
	sha256 := strings.Repeat("2", 64)
	in := "tree " + sha1 + "\nauthor A <a> 1 +0000\ncommitter A <a> 1 +0000\nx-custom value\n continued\n\nmsg\n"
	got, err := translateCommit([]byte(in), func(string) (string, error) { return sha256, nil })
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(in, sha1, sha256, 1); string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func servingRefs(t *testing.T, servingPath string) string {
	t.Helper()
	return gitLine(t, servingPath, nil, "--git-dir=.", "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags")
}

// buildHistory adds merges, annotated and nested tags, a symlink, and a symlink replaced by a
// directory to the upstream.
func buildHistory(t *testing.T, up string) {
	t.Helper()
	gitRaw(t, up, nil, "tag", "-a", "v1", "-m", "v1")
	gitRaw(t, up, nil, "-c", "advice.nestedTag=false", "tag", "-a", "v1-outer", "-m", "outer", "v1")
	gitRaw(t, up, nil, "checkout", "-q", "-b", "side")
	if err := os.Symlink("test.txt", filepath.Join(up, "link")); err != nil {
		t.Fatal(err)
	}
	gitRaw(t, up, nil, "add", "link")
	gitRaw(t, up, nil, "commit", "-q", "-m", "symlink")
	gitRaw(t, up, nil, "checkout", "-q", "main")
	commitToUpstream(t, up, "main 1")
	gitRaw(t, up, nil, "merge", "-q", "--no-ff", "-m", "merge", "side")
	gitRaw(t, up, nil, "rm", "-q", "link")
	if err := os.MkdirAll(filepath.Join(up, "link"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(up, "link", "f"), []byte("f"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRaw(t, up, nil, "add", "link/f")
	gitRaw(t, up, nil, "commit", "-q", "-m", "symlink replaced by directory")
}

func newHistory(t *testing.T) string {
	t.Helper()
	up := newTestUpstream(t)
	buildHistory(t, up)
	return up
}

func TestConversionMatchesGitOwnTranslation(t *testing.T) {
	// Git records the SHA-256 translation of every object it writes in this upstream
	up := filepath.Join(t.TempDir(), "upstream")
	gitRaw(t, "", nil, "init", "-q", "--initial-branch=main", "--object-format=sha1", up)
	gitRaw(t, up, nil, "config", "core.repositoryformatversion", "1")
	gitRaw(t, up, nil, "config", "extensions.compatObjectFormat", "sha256")
	gitRaw(t, up, nil, "config", "user.name", "Test User")
	gitRaw(t, up, nil, "config", "user.email", "test@example.com")
	if err := exec.Command("git", "-C", up, "commit", "-q", "--allow-empty", "-m", "commit 0").Run(); err != nil {
		t.Skipf("git does not support extensions.compatObjectFormat: %v", err)
	}
	commitToUpstream(t, up, "commit 1")
	buildHistory(t, up)

	info := newTestRepoInfo(t, up)
	if err := NewManager(0).EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}

	var want strings.Builder
	for _, ref := range strings.Fields(gitLine(t, up, nil, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/tags")) {
		fmt.Fprintf(&want, "%s %s\n", compatSHA256(t, up, ref), ref)
	}
	if got := servingRefs(t, info.ServingPath); got != strings.TrimSpace(want.String()) {
		t.Errorf("converted refs differ from git's own translation\ngot:\n%s\nwant:\n%s", got, want.String())
	}
	gitRaw(t, info.ServingPath, nil, "--git-dir=.", "fsck", "--strict", "--no-dangling")
	assertCompatMapComplete(t, info.ServingPath)
}

func TestConversionKeepsSignaturesModesAndTags(t *testing.T) {
	up := newTestUpstream(t)
	tree := gitLine(t, up, nil, "rev-parse", "HEAD^{tree}")
	head := gitLine(t, up, nil, "rev-parse", "HEAD")
	gitRaw(t, up, nil, "tag", "-a", "tree-tag", "-m", "tag of a tree", tree)
	// git fast-export re-creates the inner tag of a nested tag under the outer tag's name
	gitRaw(t, up, nil, "tag", "-a", "v1", "-m", "v1")
	gitRaw(t, up, nil, "-c", "advice.nestedTag=false", "tag", "-a", "v1-outer", "-m", "outer", "v1")
	blob := gitLine(t, up, nil, "rev-parse", "HEAD:test.txt")
	oddTree := gitLine(t, up, append([]byte("100664 test.txt\x00"), mustHex(t, blob)...), "hash-object", "-t", "tree", "-w", "--literally", "--stdin")
	signed := gitLine(t, up, []byte("tree "+oddTree+"\nparent "+head+"\nauthor T <t@e> 1700000000 +0000\ncommitter T <t@e> 1700000000 +0000\n"+
		"gpgsig -----BEGIN PGP SIGNATURE-----\n \n iQEz\n -----END PGP SIGNATURE-----\n\nsigned\n"), "hash-object", "-t", "commit", "-w", "--stdin")
	gitRaw(t, up, nil, "update-ref", "refs/heads/main", signed)

	info := newTestRepoInfo(t, up)
	if err := NewManager(0).EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}

	if out := gitLine(t, info.ServingPath, nil, "--git-dir=.", "cat-file", "-t", "tree-tag"); out != "tag" {
		t.Errorf("tag of a tree was not converted, got type %q", out)
	}
	outer := string(gitRaw(t, info.ServingPath, nil, "--git-dir=.", "cat-file", "tag", "v1-outer"))
	if v1 := gitLine(t, info.ServingPath, nil, "--git-dir=.", "rev-parse", "v1"); !strings.HasPrefix(outer, "object "+v1+"\n") {
		t.Errorf("nested tag v1-outer does not point to tag v1 (%s):\n%s", v1, outer)
	}
	commit := string(gitRaw(t, info.ServingPath, nil, "--git-dir=.", "cat-file", "commit", "main"))
	if !strings.Contains(commit, "gpgsig -----BEGIN PGP SIGNATURE-----\n \n iQEz\n") {
		t.Errorf("commit signature was not kept:\n%s", commit)
	}
	rawTree := gitRaw(t, info.ServingPath, nil, "--git-dir=.", "cat-file", "tree", "main^{tree}")
	if !bytes.HasPrefix(rawTree, []byte("100664 test.txt\x00")) {
		t.Errorf("non-canonical mode was not kept: %q", rawTree)
	}
	if got, err := LookupID(info.ServingPath, "sha1", signed); err != nil || got != gitLine(t, info.ServingPath, nil, "--git-dir=.", "rev-parse", "main") {
		t.Errorf("LookupID(%s) = %q, %v", signed, got, err)
	}
}

func TestIncrementalConversionMatchesFull(t *testing.T) {
	up := newHistory(t)
	incremental := newTestRepoInfo(t, up)
	m := NewManager(0)
	if err := m.EnsureRepo(context.Background(), incremental, nil); err != nil {
		t.Fatal(err)
	}
	build := gitLine(t, "", nil, "-C", incremental.ServingPath, "rev-parse", "--absolute-git-dir")

	for i := 0; i < 5; i++ {
		commitToUpstream(t, up, fmt.Sprintf("later %d", i))
	}
	gitRaw(t, up, nil, "tag", "-a", "v2", "-m", "v2")
	gitRaw(t, up, nil, "branch", "-D", "side")
	if err := m.EnsureRepo(context.Background(), incremental, nil); err != nil {
		t.Fatal(err)
	}
	if again := gitLine(t, "", nil, "-C", incremental.ServingPath, "rev-parse", "--absolute-git-dir"); again != build {
		t.Fatalf("second conversion was not incremental: %s became %s", build, again)
	}

	full := newTestRepoInfo(t, up)
	if err := NewManager(0).EnsureRepo(context.Background(), full, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := servingRefs(t, incremental.ServingPath), servingRefs(t, full.ServingPath); got != want {
		t.Errorf("incremental differs from full conversion\nincremental:\n%s\nfull:\n%s", got, want)
	}
	gitRaw(t, incremental.ServingPath, nil, "--git-dir=.", "fsck", "--strict", "--no-dangling")
	assertCompatMapComplete(t, incremental.ServingPath)
}

func packCount(t *testing.T, repoPath string) int {
	t.Helper()
	packs, err := filepath.Glob(filepath.Join(repoPath, "objects", "pack", "pack-*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	return len(packs)
}

// commitBigFileVersion commits a large file changed slightly in every version, so that a packed
// repository stores it as deltas.
func commitBigFileVersion(t *testing.T, up string, version int) {
	t.Helper()
	var content strings.Builder
	for i := 0; i < 2000; i++ {
		if i == 1000 {
			fmt.Fprintf(&content, "line %d (version %d)\n", i, version)
		} else {
			fmt.Fprintf(&content, "line %d of a file large enough to be deltified\n", i)
		}
	}
	if err := os.WriteFile(filepath.Join(up, "big.txt"), []byte(content.String()), 0644); err != nil {
		t.Fatal(err)
	}
	gitRaw(t, up, nil, "add", "big.txt")
	gitRaw(t, up, nil, "commit", "-q", "-m", fmt.Sprintf("version %d", version))
}

func hasDeltas(t *testing.T, repoPath string) bool {
	t.Helper()
	bases := gitLine(t, repoPath, nil, "--git-dir=.", "cat-file", "--batch-all-objects", "--batch-check=%(deltabase)")
	return strings.Count(bases, strings.Repeat("0", 64)) < len(strings.Fields(bases))
}

func TestChunksAreCompressed(t *testing.T) {
	up := newTestUpstream(t)
	for i := 0; i < 3; i++ {
		commitBigFileVersion(t, up, i)
	}
	target := filepath.Join(t.TempDir(), "target.git")
	gitRaw(t, "", nil, "init", "-q", "--bare", "--object-format=sha256", target)

	// Without the final repack of a conversion, which would hide uncompressed chunks
	if _, err := rewriteObjects(context.Background(), up, target, make(map[sha1ID]sha256ID), newGitlinkResolution(nil), nil); err != nil {
		t.Fatal(err)
	}
	if !hasDeltas(t, target) {
		t.Error("chunk was not compressed: no object is stored as a delta")
	}
}

func TestConversionRepacks(t *testing.T) {
	up := newTestUpstream(t)
	commitBigFileVersion(t, up, 0)
	commitBigFileVersion(t, up, 1)

	info := newTestRepoInfo(t, up)
	m := NewManager(0)
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	if n := packCount(t, info.ServingPath); n != 1 {
		t.Errorf("full conversion left %d packs, want 1", n)
	}
	if !hasDeltas(t, info.ServingPath) {
		t.Error("full conversion was not repacked: no object is stored as a delta")
	}

	for i := 2; i <= maxPacksBeforeRepack+1; i++ {
		commitBigFileVersion(t, up, i)
		if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
			t.Fatal(err)
		}
		if n := packCount(t, info.ServingPath); n > maxPacksBeforeRepack {
			t.Fatalf("after update %d the serving repository has %d packs, want at most %d", i, n, maxPacksBeforeRepack)
		}
	}
	gitRaw(t, info.ServingPath, nil, "--git-dir=.", "fsck", "--strict", "--no-dangling")
	assertCompatMapComplete(t, info.ServingPath)
}

func TestRepackKeepsUnreachableTranslatedObjects(t *testing.T) {
	up := newTestUpstream(t)
	gitRaw(t, up, nil, "checkout", "-q", "-b", "feature")
	commitToUpstream(t, up, "feature work")
	feature := gitLine(t, up, nil, "rev-parse", "HEAD")
	info := newTestRepoInfo(t, up)
	if err := NewManager(0).EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	converted, err := LookupID(info.ServingPath, "sha1", feature)
	if err != nil {
		t.Fatal(err)
	}

	// The commit stays in loose-object-idx after its branch is gone, e.g. if its backup failed
	gitRaw(t, info.ServingPath, nil, "--git-dir=.", "update-ref", "-d", "refs/heads/feature")
	if err := repackConverted(context.Background(), info.ServingPath, false, nil, "repo"); err != nil {
		t.Fatal(err)
	}
	if typ := gitLine(t, info.ServingPath, nil, "--git-dir=.", "cat-file", "-t", converted); typ != "commit" {
		t.Errorf("translated commit %s was dropped by repack", converted)
	}
}

func TestChunkedPacksGiveSameRepository(t *testing.T) {
	up := newHistory(t)
	for i := 0; i < 20; i++ {
		commitToUpstream(t, up, fmt.Sprintf("more %d", i))
	}
	whole := newTestRepoInfo(t, up)
	if err := NewManager(0).EnsureRepo(context.Background(), whole, nil); err != nil {
		t.Fatal(err)
	}

	defer func(size int64) { packChunkSize = size }(packChunkSize)
	packChunkSize = 512 // a few objects per chunk
	chunked := newTestRepoInfo(t, up)
	if err := NewManager(0).EnsureRepo(context.Background(), chunked, nil); err != nil {
		t.Fatal(err)
	}

	if got, want := servingRefs(t, chunked.ServingPath), servingRefs(t, whole.ServingPath); got != want {
		t.Errorf("chunked conversion differs\nchunked:\n%s\nwhole:\n%s", got, want)
	}
	gitRaw(t, chunked.ServingPath, nil, "--git-dir=.", "fsck", "--strict", "--no-dangling")
	assertCompatMapComplete(t, chunked.ServingPath)
	if leftovers, _ := filepath.Glob(filepath.Join(chunked.ServingPath, "objects", "pack", "tmp_*")); len(leftovers) > 0 {
		t.Errorf("temporary packs left behind: %v", leftovers)
	}
}

func TestConversionWithoutMarkerIsRebuilt(t *testing.T) {
	up := newTestUpstream(t)
	info := newTestRepoInfo(t, up)
	m := NewManager(0)
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	build := gitLine(t, "", nil, "-C", info.ServingPath, "rev-parse", "--absolute-git-dir")

	// As left by older versions, which converted with fast-export
	if err := os.Remove(filepath.Join(info.ServingPath, rewriteMarkerFile)); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	if rebuilt := gitLine(t, "", nil, "-C", info.ServingPath, "rev-parse", "--absolute-git-dir"); rebuilt == build || !isIncrementalRewrite(info.ServingPath) {
		t.Errorf("repository without marker was not rebuilt (build %s, now %s)", build, rebuilt)
	}
}
