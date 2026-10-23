package converter

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testTranslations returns n distinct SHA-1 -> SHA-256 pairs.
func testTranslations(n, seed int) map[sha1ID]sha256ID {
	ids := make(map[sha1ID]sha256ID, n)
	for i := 0; i < n; i++ {
		ids[sha1.Sum([]byte(fmt.Sprintf("object %d/%d", seed, i)))] = sha256.Sum256([]byte(fmt.Sprintf("object %d/%d", seed, i)))
	}
	return ids
}

func TestTranslationIndexesMergeAndLookUp(t *testing.T) {
	dir := t.TempDir()
	first, second := testTranslations(1000, 1), testTranslations(500, 2)
	if err := writeTranslationIndexes(dir, nil, first); err != nil {
		t.Fatal(err)
	}
	old, err := openTranslationIndex(filepath.Join(dir, sha1IndexFile), len(sha1ID{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTranslationIndexes(dir, old, second); err != nil {
		t.Fatal(err)
	}
	old.Close()

	bySHA1, err := openTranslationIndex(filepath.Join(dir, sha1IndexFile), len(sha1ID{}))
	if err != nil {
		t.Fatal(err)
	}
	defer bySHA1.Close()
	bySHA256, err := openTranslationIndex(filepath.Join(dir, sha256IndexFile), len(sha256ID{}))
	if err != nil {
		t.Fatal(err)
	}
	defer bySHA256.Close()
	if bySHA1.len() != 1500 || bySHA256.len() != 1500 {
		t.Fatalf("indexes have %d and %d records, want 1500", bySHA1.len(), bySHA256.len())
	}
	for _, ids := range []map[sha1ID]sha256ID{first, second} {
		for oldID, newID := range ids {
			if value, ok := bySHA1.lookup(oldID[:]); !ok || string(value) != string(newID[:]) {
				t.Fatalf("SHA-1 %x: got %x, %v; want %x", oldID, value, ok, newID)
			}
			if value, ok := bySHA256.lookup(newID[:]); !ok || string(value) != string(oldID[:]) {
				t.Fatalf("SHA-256 %x: got %x, %v; want %x", newID, value, ok, oldID)
			}
		}
	}
	var unknown sha1ID
	if _, ok := bySHA1.lookup(unknown[:]); ok {
		t.Error("unknown SHA-1 found")
	}

	// Prefix search: a full id has one match, a one-digit prefix many
	for oldID, newID := range first {
		if matches := bySHA1.withPrefix(fmt.Sprintf("%x", oldID)[:13], 2); len(matches) != 1 || string(matches[0]) != string(newID[:]) {
			t.Errorf("prefix of %x: %d matches", oldID, len(matches))
		}
		break
	}
	if matches := bySHA1.withPrefix("a", 2); len(matches) != 2 {
		t.Errorf("prefix \"a\": %d matches, want an ambiguous 2", len(matches))
	}
}

func TestIncrementalConversionIndexesRepositoriesWithoutIndexes(t *testing.T) {
	up := newHistory(t)
	info := newTestRepoInfo(t, up)
	m := NewManager(0)
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	build := gitLine(t, "", nil, "-C", info.ServingPath, "rev-parse", "--absolute-git-dir")

	// As converted before the indexes existed
	for _, file := range []string{sha1IndexFile, sha256IndexFile} {
		if err := os.Remove(filepath.Join(info.ServingPath, file)); err != nil {
			t.Fatal(err)
		}
	}
	commitToUpstream(t, up, "update")
	if err := m.EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	if again := gitLine(t, "", nil, "-C", info.ServingPath, "rev-parse", "--absolute-git-dir"); again != build {
		t.Errorf("repository without indexes was rebuilt instead of indexed and extended")
	}

	full := newTestRepoInfo(t, up)
	if err := NewManager(0).EnsureRepo(context.Background(), full, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := servingRefs(t, info.ServingPath), servingRefs(t, full.ServingPath); got != want {
		t.Errorf("incremental conversion differs from full conversion\ngot:\n%s\nwant:\n%s", got, want)
	}
	for _, head := range []string{gitLine(t, up, nil, "rev-parse", "HEAD"), gitLine(t, up, nil, "rev-parse", "HEAD~1")} {
		if _, err := LookupID(info.ServingPath, "sha1", head); err != nil {
			t.Errorf("LookupID(%s) after indexing: %v", head, err)
		}
	}
}

func TestLookupIDWithoutIndexScansTranslationTable(t *testing.T) {
	up := newTestUpstream(t)
	info := newTestRepoInfo(t, up)
	if err := NewManager(0).EnsureRepo(context.Background(), info, nil); err != nil {
		t.Fatal(err)
	}
	head := gitLine(t, up, nil, "rev-parse", "HEAD")
	want, err := LookupID(info.ServingPath, "sha1", head)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{sha1IndexFile, sha256IndexFile} {
		if err := os.Remove(filepath.Join(info.ServingPath, file)); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := LookupID(info.ServingPath, "sha1", head[:10]); err != nil || got != want {
		t.Errorf("LookupID without index = %q, %v; want %s", got, err, want)
	}
	if _, err := LookupID(info.ServingPath, "sha256", strings.Repeat("0", 64)); !errors.Is(err, ErrIDNotFound) {
		t.Errorf("unknown id without index: %v, want ErrIDNotFound", err)
	}
}

func TestAppendTranslationTableDropsIncompleteLine(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "objects"), 0755); err != nil {
		t.Fatal(err)
	}
	ids := &translations{added: testTranslations(2, 3)}
	var added []sha1ID
	for id := range ids.added {
		added = append(added, id)
	}
	path := filepath.Join(repo, "objects", "loose-object-idx")
	// Left by an append interrupted in the middle of a line
	if err := os.WriteFile(path, []byte("# loose-object-idx\n"+fmt.Sprintf("%x %x\n", ids.added[added[0]], added[0])+"0123abc"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := appendTranslationTable(repo, added[1:], ids); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 || strings.Contains(string(data), "0123abc") {
		t.Fatalf("loose-object-idx after append:\n%s", data)
	}
	for _, line := range lines[1:] {
		if _, _, ok := parseTranslationLine(line); !ok {
			t.Errorf("malformed line %q", line)
		}
	}
}
