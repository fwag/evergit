package converter

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
)

// A translation index is a sorted file of fixed-size records mapping the object ids of one hash
// format to the other, read through mmap with binary search, so that lookups do not load the
// translation table (11.9 million objects, 1.2 GB as loose-object-idx, for the Linux kernel).
// loose-object-idx is kept in git's format for compatibility.
const (
	sha1IndexFile   = "evergit-sha1.idx"   // records sorted by SHA-1: SHA-1, then SHA-256
	sha256IndexFile = "evergit-sha256.idx" // records sorted by SHA-256: SHA-256, then SHA-1
	indexMagic      = "EVGIDX1\n"
	indexRecordLen  = len(sha1ID{}) + len(sha256ID{})
)

type indexRecord = [indexRecordLen]byte

type translationIndex struct {
	data   []byte // the mapped file, after the magic
	mapped []byte
	keyLen int
}

// openTranslationIndex maps an index whose records start with keyLen-byte keys.
func openTranslationIndex(path string, keyLen int) (*translationIndex, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := int(fi.Size())
	if size < len(indexMagic) || (size-len(indexMagic))%indexRecordLen != 0 {
		return nil, fmt.Errorf("corrupt translation index %s", path)
	}
	x := &translationIndex{keyLen: keyLen}
	if size == len(indexMagic) {
		return x, nil
	}
	if x.mapped, err = syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_SHARED); err != nil {
		return nil, fmt.Errorf("failed to map %s: %w", path, err)
	}
	if string(x.mapped[:len(indexMagic)]) != indexMagic {
		_ = syscall.Munmap(x.mapped)
		return nil, fmt.Errorf("corrupt translation index %s", path)
	}
	x.data = x.mapped[len(indexMagic):]
	return x, nil
}

func (x *translationIndex) Close() error {
	if x == nil || x.mapped == nil {
		return nil
	}
	err := syscall.Munmap(x.mapped)
	x.mapped, x.data = nil, nil
	return err
}

func (x *translationIndex) len() int {
	return len(x.data) / indexRecordLen
}

func (x *translationIndex) record(i int) []byte {
	return x.data[i*indexRecordLen : (i+1)*indexRecordLen]
}

// search returns the index of the first record whose key is not below key.
func (x *translationIndex) search(key []byte) int {
	return sort.Search(x.len(), func(i int) bool {
		return bytes.Compare(x.record(i)[:x.keyLen], key) >= 0
	})
}

// lookup returns the value recorded for key.
func (x *translationIndex) lookup(key []byte) ([]byte, bool) {
	if x == nil {
		return nil, false
	}
	if i := x.search(key); i < x.len() && bytes.Equal(x.record(i)[:x.keyLen], key) {
		return x.record(i)[x.keyLen:], true
	}
	return nil, false
}

// withPrefix returns the values of up to limit records whose hexadecimal key starts with prefix.
func (x *translationIndex) withPrefix(prefix string, limit int) [][]byte {
	if x == nil {
		return nil
	}
	padded := prefix + strings.Repeat("0", 2*x.keyLen-len(prefix))
	low, err := hex.DecodeString(padded)
	if err != nil {
		return nil
	}
	var values [][]byte
	for i := x.search(low); i < x.len() && len(values) < limit; i++ {
		if !strings.HasPrefix(hex.EncodeToString(x.record(i)[:x.keyLen]), prefix) {
			break
		}
		values = append(values, x.record(i)[x.keyLen:])
	}
	return values
}

// translations are the SHA-1 -> SHA-256 translations of a conversion: those it adds, and those of
// earlier conversions in the serving repository's index.
type translations struct {
	added map[sha1ID]sha256ID
	known *translationIndex // nil for a full conversion
}

func (t *translations) lookup(id sha1ID) (sha256ID, bool) {
	if newID, ok := t.added[id]; ok {
		return newID, true
	}
	var newID sha256ID
	value, ok := t.known.lookup(id[:])
	copy(newID[:], value)
	return newID, ok
}

// writeTranslationIndexes writes both indexes of repoPath: the records of old (an earlier sha1
// index, or nil) merged with added. Each index is written to a temporary file and renamed.
func writeTranslationIndexes(repoPath string, old *translationIndex, added map[sha1ID]sha256ID) error {
	records := make([]indexRecord, 0, len(added))
	for oldID, newID := range added {
		var r indexRecord
		copy(r[:], oldID[:])
		copy(r[len(oldID):], newID[:])
		records = append(records, r)
	}
	slices.SortFunc(records, func(a, b indexRecord) int { return bytes.Compare(a[:], b[:]) })
	var oldRecords func(i int) []byte
	oldLen := 0
	if old != nil {
		oldRecords, oldLen = old.record, old.len()
	}
	if err := writeMergedIndex(filepath.Join(repoPath, sha1IndexFile), len(sha1ID{}), oldRecords, oldLen, records); err != nil {
		return err
	}

	// The SHA-256 index of the same records: swap the halves and sort again
	for i, r := range records {
		var swapped indexRecord
		copy(swapped[:], r[len(sha1ID{}):])
		copy(swapped[len(sha256ID{}):], r[:len(sha1ID{})])
		records[i] = swapped
	}
	slices.SortFunc(records, func(a, b indexRecord) int { return bytes.Compare(a[:], b[:]) })
	var oldSHA256 *translationIndex
	if old != nil {
		var err error
		if oldSHA256, err = openTranslationIndex(filepath.Join(repoPath, sha256IndexFile), len(sha256ID{})); err != nil {
			return fmt.Errorf("failed to open the SHA-256 index: %w", err)
		}
		defer oldSHA256.Close()
		oldRecords, oldLen = oldSHA256.record, oldSHA256.len()
	}
	return writeMergedIndex(filepath.Join(repoPath, sha256IndexFile), len(sha256ID{}), oldRecords, oldLen, records)
}

// writeMergedIndex writes the sorted union of oldLen records read by old and the sorted added
// records. A key present in both keeps the added record.
func writeMergedIndex(path string, keyLen int, old func(i int) []byte, oldLen int, added []indexRecord) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	w := bufio.NewWriterSize(f, 1<<20)
	_, _ = w.WriteString(indexMagic)
	i, j := 0, 0
	for i < oldLen || j < len(added) {
		switch {
		case j == len(added) || (i < oldLen && bytes.Compare(old(i)[:keyLen], added[j][:keyLen]) < 0):
			_, _ = w.Write(old(i))
			i++
		default:
			if i < oldLen && bytes.Equal(old(i)[:keyLen], added[j][:keyLen]) {
				i++
			}
			_, _ = w.Write(added[j][:])
			j++
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ensureTranslationIndexes generates the indexes from loose-object-idx if they are missing, e.g. for
// a repository converted before they existed.
func ensureTranslationIndexes(repoPath string) error {
	_, err1 := os.Stat(filepath.Join(repoPath, sha1IndexFile))
	_, err2 := os.Stat(filepath.Join(repoPath, sha256IndexFile))
	if err1 == nil && err2 == nil {
		return nil
	}
	f, err := os.Open(filepath.Join(repoPath, "objects", "loose-object-idx"))
	if err != nil {
		return err
	}
	defer f.Close()
	added := make(map[sha1ID]sha256ID)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if oldID, newID, ok := parseTranslationLine(scanner.Text()); ok {
			added[oldID] = newID
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return writeTranslationIndexes(repoPath, nil, added)
}

// parseTranslationLine parses a "<sha256> <sha1>" line of loose-object-idx.
func parseTranslationLine(line string) (oldID sha1ID, newID sha256ID, ok bool) {
	newHex, oldHex, found := strings.Cut(line, " ")
	if !found || len(newHex) != 2*len(newID) || len(oldHex) != 2*len(oldID) {
		return oldID, newID, false
	}
	if _, err := hex.Decode(newID[:], []byte(newHex)); err != nil {
		return oldID, newID, false
	}
	if _, err := hex.Decode(oldID[:], []byte(oldHex)); err != nil {
		return oldID, newID, false
	}
	return oldID, newID, true
}

// appendTranslationTable appends added to loose-object-idx, creating it if needed. A line left
// incomplete by an interrupted append is removed first.
func appendTranslationTable(repoPath string, added []sha1ID, ids *translations) error {
	path := filepath.Join(repoPath, "objects", "loose-object-idx")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if size == 0 {
		if _, err := f.WriteString("# loose-object-idx\n"); err != nil {
			return err
		}
	} else if err := truncateToLastLine(f, size); err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	for _, id := range added {
		newID, _ := ids.lookup(id)
		fmt.Fprintf(w, "%x %x\n", newID, id)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Sync()
}

// truncateToLastLine drops a final line without newline and leaves the offset at the end.
func truncateToLastLine(f *os.File, size int64) error {
	buf := make([]byte, 1)
	for end := size; end > 0; end-- {
		if _, err := f.ReadAt(buf, end-1); err != nil {
			return err
		}
		if buf[0] == '\n' {
			if end != size {
				if err := f.Truncate(end); err != nil {
					return err
				}
			}
			_, err := f.Seek(end, io.SeekStart)
			return err
		}
	}
	return errors.New("loose-object-idx has no complete line")
}
