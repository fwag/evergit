package converter

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"evergit/internal/resolver"
)

// Object-level conversion implements the translation defined by Git's hash function transition
// design (Documentation/technical/hash-function-transition.txt): the SHA-256 content of an object
// is its SHA-1 content with every referenced object named by its SHA-256 id. All other bytes,
// including signatures, are kept, so the result matches what Git's own compatObjectFormat code
// computes and does not depend on the installed git version (unlike fast-export | fast-import,
// which re-creates objects and drops signatures, unknown headers and tags of trees).

type sha1ID = [20]byte
type sha256ID = [32]byte

// signatureMarkers are the lines that start an in-body tag signature (git's gpg-interface.c).
var signatureMarkers = []string{
	"-----BEGIN PGP SIGNATURE-----",
	"-----BEGIN PGP MESSAGE-----",
	"-----BEGIN SIGNED MESSAGE-----",
	"-----BEGIN SSH SIGNATURE-----",
}

// errObjectMissing is returned, as a *missingObjectError, for an object the repository does not contain.
var errObjectMissing = errors.New("object missing")

type missingObjectError struct{ id sha1ID }

func (e *missingObjectError) Error() string        { return fmt.Sprintf("%v: %x", errObjectMissing, e.id) }
func (e *missingObjectError) Is(target error) bool { return target == errObjectMissing }

// cutLine splits off the first line of data, including its newline if present.
func cutLine(data []byte) (line, rest []byte) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return data[:i+1], data[i+1:]
	}
	return data, nil
}

// translateTree rewrites the object id of every "<mode> <name>\0<id>" entry. Modes and names are
// copied verbatim, including non-canonical modes such as 100664.
func translateTree(data []byte, mapID func(mode, name []byte, id sha1ID) (sha256ID, error)) ([]byte, error) {
	out := make([]byte, 0, len(data)+len(data)/2)
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		nul := bytes.IndexByte(data, 0)
		if sp <= 0 || nul < sp || len(data) < nul+1+len(sha1ID{}) {
			return nil, errors.New("malformed tree entry")
		}
		var id sha1ID
		copy(id[:], data[nul+1:])
		newID, err := mapID(data[:sp], data[sp+1:nul], id)
		if err != nil {
			return nil, err
		}
		out = append(out, data[:nul+1]...)
		out = append(out, newID[:]...)
		data = data[nul+1+len(id):]
	}
	return out, nil
}

// translateCommit rewrites the tree and parent headers and the tag embedded in mergetag headers,
// whose object reference is translated by mapMergetag. Other headers (author, encoding, gpgsig, ...)
// and the message are copied verbatim. Unlike git, which refuses to convert commits with unknown
// headers, those are copied verbatim too, as the transition design defines the content to be
// unchanged apart from object references.
func translateCommit(data []byte, mapHex, mapMergetag func(string) (string, error)) ([]byte, error) {
	var out bytes.Buffer
	out.Grow(len(data) + 64)
	for len(data) > 0 && data[0] != '\n' {
		line, rest := cutLine(data)
		data = rest
		switch {
		case bytes.HasPrefix(line, []byte("tree ")) || bytes.HasPrefix(line, []byte("parent ")):
			key, value, _ := bytes.Cut(bytes.TrimSuffix(line, []byte("\n")), []byte(" "))
			mapped, err := mapHex(string(value))
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(&out, "%s %s\n", key, mapped)
		case bytes.HasPrefix(line, []byte("mergetag ")):
			tag := append([]byte(nil), line[len("mergetag "):]...)
			for len(data) > 0 && data[0] == ' ' {
				line, data = cutLine(data)
				tag = append(tag, line[1:]...)
			}
			newTag, err := translateTag(tag, mapMergetag)
			if err != nil {
				return nil, fmt.Errorf("mergetag: %w", err)
			}
			out.WriteString("mergetag")
			for len(newTag) > 0 {
				line, newTag = cutLine(newTag)
				out.WriteByte(' ')
				out.Write(line)
				if !bytes.HasSuffix(line, []byte("\n")) {
					out.WriteByte('\n')
				}
			}
		default:
			out.Write(line)
		}
	}
	out.Write(data)
	return out.Bytes(), nil
}

// translateTag rewrites the object header. As in git, the in-body signature belongs to the hash
// the object is named with, so a SHA-1 signature moves into a "gpgsig" header and a
// "gpgsig-sha256" header becomes the in-body signature.
func translateTag(data []byte, mapHex func(string) (string, error)) ([]byte, error) {
	line, rest := cutLine(data)
	if !bytes.HasPrefix(line, []byte("object ")) || !bytes.HasSuffix(line, []byte("\n")) {
		return nil, errors.New("tag does not start with an object header")
	}
	mapped, err := mapHex(string(line[len("object ") : len(line)-1]))
	if err != nil {
		return nil, err
	}

	payloadLen := signedPayloadLen(rest)
	ourSig := rest[payloadLen:]
	payload, otherSig := extractHeaderSignature(rest[:payloadLen], "gpgsig-sha256")

	out := append([]byte("object "+mapped+"\n"), payload...)
	if len(ourSig) > 0 {
		out = insertHeaderSignature(out, ourSig, "gpgsig")
	}
	return append(out, otherSig...), nil
}

// signedPayloadLen returns the offset of the last line starting a signature, or len(data).
func signedPayloadLen(data []byte) int {
	match := len(data)
	for pos := 0; pos < len(data); {
		for _, marker := range signatureMarkers {
			if bytes.HasPrefix(data[pos:], []byte(marker)) {
				match = pos
				break
			}
		}
		line, _ := cutLine(data[pos:])
		pos += len(line)
	}
	return match
}

// extractHeaderSignature removes the multi-line header named header from the header section of
// data, returning the remaining payload and the header value without its continuation spaces.
func extractHeaderSignature(data []byte, header string) (payload, sig []byte) {
	prefix := []byte(header + " ")
	inSig := false
	for len(data) > 0 {
		if data[0] == '\n' {
			return append(payload, data...), sig
		}
		line, rest := cutLine(data)
		data = rest
		switch {
		case inSig && line[0] == ' ':
			sig = append(sig, line[1:]...)
		case bytes.HasPrefix(line, prefix):
			sig = append(sig, line[len(prefix):]...)
			inSig = true
		default:
			payload = append(payload, line...)
			inSig = false
		}
	}
	return payload, sig
}

// insertHeaderSignature adds sig as a multi-line header at the end of the header section of buf.
func insertHeaderSignature(buf, sig []byte, header string) []byte {
	pos := len(buf)
	if i := bytes.Index(buf, []byte("\n\n")); i >= 0 {
		pos = i + 1
	}
	var h []byte
	for first := true; len(sig) > 0; first = false {
		var line []byte
		line, sig = cutLine(sig)
		if first {
			h = append(h, header...)
		}
		h = append(h, ' ')
		h = append(h, line...)
	}
	return slices.Concat(buf[:pos], h, buf[pos:])
}

func hashObject(typ string, data []byte) sha256ID {
	h := sha256.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	var id sha256ID
	h.Sum(id[:0])
	return id
}

func parseSHA1Hex(s string) (sha1ID, error) {
	var id sha1ID
	if len(s) != 2*len(id) {
		return id, fmt.Errorf("invalid SHA-1 object id %q", s)
	}
	if _, err := hex.Decode(id[:], []byte(s)); err != nil {
		return id, fmt.Errorf("invalid SHA-1 object id %q", s)
	}
	return id, nil
}

// catFileBatch reads objects through a single long-running "git cat-file --batch".
type catFileBatch struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr strings.Builder
}

func startCatFile(ctx context.Context, repoPath string) (*catFileBatch, error) {
	c := &catFileBatch{cmd: exec.CommandContext(ctx, "git", "cat-file", "--batch")}
	c.cmd.Dir = repoPath
	c.cmd.Env = GitEnv()
	c.cmd.Stderr = &c.stderr
	in, err := c.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := c.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start git cat-file: %w", err)
	}
	c.in, c.out = in, bufio.NewReaderSize(out, 1<<20)
	return c, nil
}

func (c *catFileBatch) read(id sha1ID) (string, []byte, error) {
	if _, err := fmt.Fprintf(c.in, "%x\n", id); err != nil {
		return "", nil, fmt.Errorf("git cat-file: %w (stderr: %q)", err, c.stderr.String())
	}
	header, err := c.out.ReadString('\n')
	if err != nil {
		return "", nil, fmt.Errorf("git cat-file: %w (stderr: %q)", err, c.stderr.String())
	}
	fields := strings.Fields(header)
	if len(fields) == 2 && fields[1] == "missing" {
		return "", nil, &missingObjectError{id: id}
	}
	if len(fields) != 3 {
		return "", nil, fmt.Errorf("object %x: %s", id, strings.TrimSpace(header))
	}
	size, err := strconv.Atoi(fields[2])
	if err != nil || size < 0 {
		return "", nil, fmt.Errorf("object %x: bad size in %q", id, header)
	}
	data := make([]byte, size+1) // object content is followed by a newline
	if _, err := io.ReadFull(c.out, data); err != nil {
		return "", nil, fmt.Errorf("object %x: %w", id, err)
	}
	return fields[1], data[:size], nil
}

func (c *catFileBatch) close() {
	_ = c.in.Close()
	_ = c.cmd.Wait()
}

var packObjectTypes = map[string]byte{"commit": 1, "tree": 2, "blob": 3, "tag": 4}

// packChunkSize bounds the undeltified pack written before git pack-objects compresses it, and so
// the extra disk space a conversion needs on top of the converted repository.
var packChunkSize int64 = 1 << 30

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// packWriter writes undeltified objects to a version 2 packfile with a SHA-256 trailer.
type packWriter struct {
	base  string // path without the .pack extension
	f     *os.File
	cw    *countingWriter
	w     *bufio.Writer
	zw    *zlib.Writer
	count uint32
}

func newPackWriter(base string) (*packWriter, error) {
	f, err := os.Create(base + ".pack")
	if err != nil {
		return nil, err
	}
	p := &packWriter{base: base, f: f, cw: &countingWriter{w: f}}
	p.w = bufio.NewWriterSize(p.cw, 1<<20)
	p.zw, _ = zlib.NewWriterLevel(p.w, zlib.BestSpeed)
	// The object count is patched in by finish
	if _, err := p.w.WriteString("PACK\x00\x00\x00\x02\x00\x00\x00\x00"); err != nil {
		f.Close()
		return nil, err
	}
	return p, nil
}

func (p *packWriter) size() int64 {
	return p.cw.n + int64(p.w.Buffered())
}

func (p *packWriter) add(typ string, data []byte) error {
	size := uint64(len(data))
	b := packObjectTypes[typ]<<4 | byte(size&0x0f)
	size >>= 4
	var hdr []byte
	for size > 0 {
		hdr = append(hdr, b|0x80)
		b = byte(size & 0x7f)
		size >>= 7
	}
	hdr = append(hdr, b)
	if _, err := p.w.Write(hdr); err != nil {
		return err
	}
	p.zw.Reset(p.w)
	if _, err := p.zw.Write(data); err != nil {
		return err
	}
	if err := p.zw.Close(); err != nil {
		return err
	}
	p.count++
	return nil
}

// finish patches the object count, appends the SHA-256 trailer and returns the pack checksum.
func (p *packWriter) finish() (string, error) {
	defer p.f.Close()
	if err := p.w.Flush(); err != nil {
		return "", err
	}
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], p.count)
	if _, err := p.f.WriteAt(count[:], 8); err != nil {
		return "", err
	}
	if _, err := p.f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, p.f); err != nil {
		return "", err
	}
	sum := h.Sum(nil)
	if _, err := p.f.Write(sum); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum), p.f.Close()
}

// discard removes the pack and any index built for it.
func (p *packWriter) discard() {
	_ = p.f.Close()
	for _, ext := range []string{".idx", ".pack", ".rev"} {
		_ = os.Remove(p.base + ext)
	}
}

// packedObject is a converted object awaiting compression, with the file name pack-objects uses to
// pair it with similar objects as delta candidates.
type packedObject struct {
	id   sha256ID
	name string
}

// objectRewriter converts objects depth-first, so every object is written after the objects it
// references.
type objectRewriter struct {
	ctx      context.Context
	cat      *catFileBatch
	repoPath string
	packDir  string
	pack     *packWriter    // nil when only computing ids
	chunk    []packedObject // objects in pack
	ids      *translations
	added    []sha1ID // objects converted in this run, in conversion order
	gitlinks *gitlinkResolution
	progress func(converted int)

	// missingMergetags maps objects that mergetags refer to but the repository lacks (e.g. the inner
	// tag of a tag of a tag, which stayed in the maintainer's repository) to a merge recording one
	missingMergetags map[string]string
}

func (r *objectRewriter) convert(id sha1ID, name string) (sha256ID, error) {
	if newID, ok := r.ids.lookup(id); ok {
		return newID, nil
	}
	typ, data, err := r.cat.read(id)
	if err != nil {
		return sha256ID{}, err
	}
	switch typ {
	case "blob":
	case "tree":
		data, err = translateTree(data, r.convertTreeEntry)
	case "commit":
		data, err = translateCommit(data, r.convertHex, func(s string) (string, error) { return r.convertMergetagHex(s, id) })
	case "tag":
		data, err = translateTag(data, r.convertHex)
	default:
		err = fmt.Errorf("unknown object type %q", typ)
	}
	if err != nil {
		return sha256ID{}, fmt.Errorf("failed to convert %s %x: %w", typ, id, err)
	}
	newID := hashObject(typ, data)
	r.ids.added[id] = newID
	r.added = append(r.added, id)
	if r.progress != nil && len(r.added)%progressObjectInterval == 0 {
		r.progress(len(r.added))
	}
	if r.pack == nil {
		return newID, nil // only computing ids
	}
	if err := r.pack.add(typ, data); err != nil {
		return sha256ID{}, fmt.Errorf("failed to write %s %x to pack: %w", typ, id, err)
	}
	r.chunk = append(r.chunk, packedObject{id: newID, name: name})
	if r.pack.size() >= packChunkSize {
		if err := r.flushChunk(); err != nil {
			return sha256ID{}, err
		}
		if err := r.startChunk(); err != nil {
			return sha256ID{}, err
		}
	}
	return newID, nil
}

func (r *objectRewriter) convertTreeEntry(mode, name []byte, id sha1ID) (sha256ID, error) {
	if string(mode) != "160000" {
		return r.convert(id, string(name))
	}
	newID, ok, err := r.gitlinks.resolve(id)
	if err != nil {
		return sha256ID{}, err
	}
	if !ok {
		// A submodule pointing into this repository
		if newID, err = r.convert(id, ""); errors.Is(err, errObjectMissing) {
			return sha256ID{}, fmt.Errorf("no SHA-256 was decided for submodule commit %x", id)
		} else if err != nil {
			return sha256ID{}, err
		}
	}
	r.gitlinks.decided[id] = newID
	return newID, nil
}

// convertMergetagHex translates the object of the tag embedded in a mergetag of merge. A mergetag is
// a copy of a signed tag kept for verification, and git does not require its object: if the
// repository lacks it, no copy of the repository has it, so it gets the synthetic SHA-256
// (<sha1> + 24 zeros), as git itself cannot convert such a commit.
func (r *objectRewriter) convertMergetagHex(s string, merge sha1ID) (string, error) {
	newHex, err := r.convertHex(s)
	var missing *missingObjectError
	if !errors.As(err, &missing) || hex.EncodeToString(missing.id[:]) != s {
		return newHex, err // translated, or another object is missing (a corrupt repository)
	}
	if r.missingMergetags == nil {
		r.missingMergetags = make(map[string]string)
	}
	r.missingMergetags[s] = hex.EncodeToString(merge[:])
	return s + syntheticSHA256Padding, nil
}

func (r *objectRewriter) convertHex(s string) (string, error) {
	id, err := parseSHA1Hex(s)
	if err != nil {
		return "", err
	}
	newID, err := r.convert(id, "")
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(newID[:]), nil
}

func (r *objectRewriter) startChunk() error {
	pack, err := newPackWriter(filepath.Join(r.packDir, fmt.Sprintf("tmp_evergit_%d", time.Now().UnixNano())))
	if err != nil {
		return fmt.Errorf("failed to create pack: %w", err)
	}
	r.pack, r.chunk = pack, nil
	return nil
}

// flushChunk installs the undeltified pack and replaces it with one compressed by git pack-objects,
// so that at most one undeltified pack exists at a time. Deltas are only found within the chunk.
// --no-reuse-delta is required: otherwise pack-objects copies already packed objects as they are.
func (r *objectRewriter) flushChunk() error {
	if len(r.chunk) == 0 {
		r.pack.discard()
		return nil
	}
	sum, err := r.pack.finish()
	if err != nil {
		return fmt.Errorf("failed to finish pack: %w", err)
	}
	if _, err := runCmdContext(r.ctx, 0, r.repoPath, "git", "--git-dir=.", "index-pack", r.pack.base+".pack"); err != nil {
		return err
	}
	installed := &packWriter{base: filepath.Join(r.packDir, "pack-"+sum)}
	// The .idx makes the pack visible, so it is installed last
	for _, ext := range []string{".pack", ".rev", ".idx"} {
		if err := os.Rename(r.pack.base+ext, installed.base+ext); err != nil && !(ext == ".rev" && errors.Is(err, os.ErrNotExist)) {
			return fmt.Errorf("failed to install pack: %w", err)
		}
	}

	var objects strings.Builder
	for _, o := range r.chunk {
		fmt.Fprintf(&objects, "%x %s\n", o.id, o.name)
	}
	cmd := exec.CommandContext(r.ctx, "git", "--git-dir=.", "pack-objects", "-q", "--delta-base-offset", "--no-reuse-delta", filepath.Join("objects", "pack", "pack"))
	cmd.Env = GitEnv()
	cmd.Dir = r.repoPath
	cmd.Stdin = strings.NewReader(objects.String())
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git pack-objects failed in %s: %w (stderr: %q)", r.repoPath, err, strings.TrimSpace(stderr.String()))
	}
	if strings.TrimSpace(string(out)) != sum {
		installed.discard()
	}
	r.chunk = nil
	return nil
}

// rewriteObjects converts every object reachable from heads (commits, or all of the mirror's refs if
// nil) that is not yet in ids into packs in targetPath, walking only history not reachable from
// exclude (refs converted before). With an empty targetPath, it only computes the SHA-256 ids into
// ids. It returns the converted ids in conversion order, and the objects mergetags refer to but the
// repository lacks, with a merge recording each.
func rewriteObjects(ctx context.Context, mirrorPath, targetPath string, ids *translations, gitlinks *gitlinkResolution, heads, exclude []string, progress func(int)) ([]sha1ID, map[string]string, error) {
	// Parents before children keeps the recursion in convert shallow: commits only recurse into trees
	args, stdin := historyRevisions(heads, exclude)
	commits, err := runCmdStdin(ctx, 0, mirrorPath, stdin, "git", append([]string{"rev-list", "--reverse", "--topo-order"}, args...)...)
	if err != nil {
		return nil, nil, err
	}
	objects := strings.Fields(commits)
	if heads == nil {
		// Tags, and refs pointing directly at trees or blobs, are not listed by rev-list
		refTargets, err := runCmdContext(ctx, 0, mirrorPath, "git", "for-each-ref", "--format=%(objectname)")
		if err != nil {
			return nil, nil, err
		}
		objects = append(objects, strings.Fields(refTargets)...)
	}

	cat, err := startCatFile(ctx, mirrorPath)
	if err != nil {
		return nil, nil, err
	}
	defer cat.close()

	r := &objectRewriter{ctx: ctx, cat: cat, ids: ids, gitlinks: gitlinks, progress: progress}
	if targetPath != "" {
		r.repoPath, r.packDir = targetPath, filepath.Join(targetPath, "objects", "pack")
		if err := os.MkdirAll(r.packDir, 0755); err != nil {
			return nil, nil, err
		}
		if err := r.startChunk(); err != nil {
			return nil, nil, err
		}
		defer func() { r.pack.discard() }()
	}

	for _, hexID := range objects {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if _, err := r.convertHex(hexID); err != nil {
			return nil, nil, err
		}
	}
	if r.pack != nil {
		if err := r.flushChunk(); err != nil {
			return nil, nil, err
		}
	}
	return r.added, r.missingMergetags, nil
}

// historyRevisions selects the history of heads, or of all refs if heads is nil, not reachable from
// exclude: the arguments of git log or rev-list, and the revisions for their --stdin. Passed as
// arguments, the refs of a large repository would exceed the system's argument size limit.
func historyRevisions(heads, exclude []string) ([]string, io.Reader) {
	args := []string{"--stdin"}
	if heads == nil {
		args = append(args, "--all")
	}
	var b strings.Builder
	seen := make(map[string]bool, len(exclude))
	for _, id := range exclude {
		if !seen[id] {
			seen[id] = true
			b.WriteString("^" + id + "\n")
		}
	}
	for _, id := range heads {
		b.WriteString(id + "\n")
	}
	return args, strings.NewReader(b.String())
}

// updateRefs points every ref of the target at the translation of the mirror's ref.
func updateRefs(ctx context.Context, refs map[string]string, targetPath string, ids *translations) error {
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	slices.Sort(names)

	var updates strings.Builder
	for _, name := range names {
		id, err := parseSHA1Hex(refs[name])
		if err != nil {
			return err
		}
		newID, ok := ids.lookup(id)
		if !ok {
			return fmt.Errorf("ref %s points to unconverted object %x", name, id)
		}
		fmt.Fprintf(&updates, "update %s %x\n", name, newID)
	}
	if updates.Len() == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "--git-dir=.", "update-ref", "--stdin")
	cmd.Env = GitEnv()
	cmd.Dir = targetPath
	cmd.Stdin = strings.NewReader(updates.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to update refs in %s: %w (output: %q)", targetPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// fullRepackRatio triggers a full repack of a serving repository once its other packs together
// reach this fraction of its largest pack.
const fullRepackRatio = 0.25

// repackConverted keeps the packs of a conversion compact. A full build is merged into one pack with
// a reachability bitmap before it is published; its chunks are already compressed, so their deltas
// are kept. After an incremental conversion, the new pack is merged geometrically with the other
// small packs, leaving the large ones untouched, as git does on servers. Objects of an update are
// only stored as deltas against objects of the same update, so once the small packs grow to
// fullRepackRatio of the largest one, everything is repacked with a new delta search (-f), which
// takes long on large repositories but runs while the current copy is served.
//
// -k keeps objects no ref reaches any more: loose-object-idx still lists them, and incremental
// conversions skip listed objects, so a dropped object that became reachable again would be missing.
// Object ids are unaffected.
func repackConverted(ctx context.Context, repoPath string, incremental bool, progressWriter io.Writer, repoName string) error {
	args := []string{"--git-dir=.", "repack", "-a", "-d", "-k", "-q"}
	if incremental {
		packs, err := filepath.Glob(filepath.Join(repoPath, "objects", "pack", "pack-*.pack"))
		if err != nil {
			return err
		}
		var largest, total int64
		for _, pack := range packs {
			fi, err := os.Stat(pack)
			if err != nil {
				return err
			}
			total += fi.Size()
			largest = max(largest, fi.Size())
		}
		switch {
		case len(packs) <= 1:
			return nil
		case float64(total-largest) < fullRepackRatio*float64(largest):
			// The geometric merge only packs whole packs, so it never drops objects
			args = []string{"--git-dir=.", "-c", "repack.writeBitmaps=false", "repack", "-d", "-q", "--geometric=2"}
		default:
			args = append(args, "-f")
		}
	}
	if progressWriter != nil {
		_, _ = fmt.Fprintf(progressWriter, "remote: Evergit: [%s] Compressing converted objects...\n", repoName)
	}
	if _, err := runCmdContext(ctx, 0, repoPath, "git", args...); err != nil {
		return fmt.Errorf("failed to repack %s: %w", repoPath, err)
	}
	return nil
}

// isIncrementalRewrite reports whether a serving repository is a complete conversion with the
// current rules, which can be extended. Anything else (another conversion version, a repository
// converted by older versions with fast-export, a lost translation table) is rebuilt.
func isIncrementalRewrite(servingPath string) bool {
	if currentState(servingPath) == nil {
		return false
	}
	for _, file := range []string{"HEAD", filepath.Join("objects", "loose-object-idx")} {
		if _, err := os.Stat(filepath.Join(servingPath, file)); err != nil {
			return false
		}
	}
	return true
}

// convertObjects converts the mirror with object-level translation, incrementally when the serving
// repository was produced the same way.
func (m *Manager) convertObjects(ctx context.Context, info *resolver.RepositoryInfo, progressWriter io.Writer, gitlinks *gitlinkResolution) (err error) {
	if err := os.MkdirAll(filepath.Dir(info.ServingPath), 0755); err != nil {
		return fmt.Errorf("failed to create repos directory: %w", err)
	}

	incremental := isIncrementalRewrite(info.ServingPath)
	start := time.Now()
	previous, _ := loadState(info.ServingPath)
	state := newRepoState(previous)
	targetPath := info.ServingPath
	ids := &translations{added: make(map[sha1ID]sha256ID)}
	var exclude []string
	if incremental {
		log.Printf("Starting incremental object conversion for repository %s...", info.RepoName)
		if err := ensureTranslationIndexes(info.ServingPath); err != nil {
			return fmt.Errorf("failed to index loose-object-idx: %w", err)
		}
		if ids.known, err = openTranslationIndex(filepath.Join(info.ServingPath, sha1IndexFile), len(sha1ID{})); err != nil {
			return err
		}
		defer ids.known.Close()
		for _, id := range previous.ConvertedRefs {
			exclude = append(exclude, id)
		}
	} else {
		log.Printf("Starting full object conversion for repository %s...", info.RepoName)
		targetPath = filepath.Join(info.BuildsPath, strconv.FormatInt(time.Now().UnixNano(), 10))
		if _, err := runCmdContext(ctx, 0, "", "git", "init", "--bare", "--object-format=sha256", targetPath); err != nil {
			return fmt.Errorf("failed to initialize bare sha256 repository: %w", err)
		}
		defer func() {
			if err != nil {
				os.RemoveAll(targetPath)
			}
		}()
	}
	if progressWriter != nil {
		_, _ = fmt.Fprintf(progressWriter, "remote: Evergit: [%s] Converting SHA-1 objects to SHA-256...\n", info.RepoName)
	}
	progress := func(n int) {
		if progressWriter != nil {
			_, _ = fmt.Fprintf(progressWriter, "\rremote: Evergit: [Progress] Converting %s: %d objects...", info.RepoName, n)
		}
	}

	refs, err := listRefs(info.MirrorPath) // the mirror cannot change while the repository is locked
	if err != nil {
		return err
	}
	added, missingMergetags, err := rewriteObjects(ctx, info.MirrorPath, targetPath, ids, gitlinks, nil, exclude, progress)
	if err != nil {
		return err
	}
	if len(missingMergetags) > 0 {
		log.Printf("[%s] %d mergetag object(s) missing from the repository recorded with the synthetic SHA-256", info.RepoName, len(missingMergetags))
	}
	if progressWriter != nil {
		_, _ = fmt.Fprintf(progressWriter, "\rremote: Evergit: [Progress] Converting %s: %d objects, done.\n", info.RepoName, len(added))
	}

	// Translations are written before refs, so no served commit lacks its SHA-1 id
	if err := appendTranslationTable(targetPath, added, ids); err != nil {
		return fmt.Errorf("failed to write loose-object-idx: %w", err)
	}
	state.recordGitlinks(gitlinks)
	state.recordMissingMergetags(missingMergetags)
	if err := state.save(targetPath); err != nil {
		return fmt.Errorf("failed to record conversion state: %w", err)
	}
	if err := updateRefs(ctx, refs, targetPath, ids); err != nil {
		return err
	}
	if incremental {
		if err := pruneServingRefs(info); err != nil {
			return err
		}
	}
	if err := writeTranslationIndexes(targetPath, ids.known, ids.added); err != nil {
		return fmt.Errorf("failed to write translation indexes: %w", err)
	}
	// Repacking after the refs are set orders the pack and its bitmap by reachability
	if err := repackConverted(ctx, targetPath, incremental, progressWriter, info.RepoName); err != nil {
		return err
	}
	// The conversion is complete: until this is recorded, the next request converts again
	state.ConvertedRefs = refs
	if err := state.save(targetPath); err != nil {
		return fmt.Errorf("failed to record conversion state: %w", err)
	}
	if err := m.publishConversion(info, targetPath, incremental); err != nil {
		return err
	}
	kind := "full"
	if incremental {
		kind = "incremental"
	}
	log.Printf("Completed %s object conversion for repository %s in %v (%d objects)", kind, info.RepoName, time.Since(start).Round(time.Millisecond), len(added))
	return nil
}
