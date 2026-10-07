package zkcircuit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// on-disk layout inside the data directory:
//
//	data.json    – the committed state (JSON envelope, atomically replaced)
//	data.json.tmp– staging file for the next commit (rename(2) target)
//	lock         – flock file serializing concurrent processes
//
// The envelope carries an explicit format version: files written by an
// incompatible future build are refused rather than silently migrated or
// overwritten.
const (
	dirDataFile = "data.json"
	dirTmpFile  = "data.json.tmp"
	dirLockFile = "lock"

	// FormatVersion is the on-disk format this build writes and accepts.
	FormatVersion = 1
)

// envelope is the persisted state container.
//
// Circuits are kept as raw JSON during decoding so persistCircuit's strict
// UnmarshalJSON — rather than encoding/json's silent zero-value filling —
// decides what a committed circuit record may look like. UnmarshalJSON first
// walks the token stream (scanEnvelopeDuplicates) and rejects a repeated
// object key anywhere encoding/json would otherwise keep the last value. At
// the envelope's top level that comparison is case-insensitive for the five
// known fields (format/circuits/setups/jobs/artifacts): a second spelling
// that differs only in ASCII letter case — including one JSON-escaped — names
// the same field and corrupts the read instead of overriding it. A spelling
// that Unicode case-folds onto one of those fields yet is not an ASCII case
// variant ("circuitſ" with U+017F long s, directly written or ſ-escaped)
// is likewise corruption even when it is the only spelling present, since
// encoding/json's own tag matching would fill the field from it; a later
// "circuitſ": [] must never read the circuits as empty and drop them on the
// next commit.
type envelope struct {
	Format    int               `json:"format"`
	Circuits  []persistCircuit  `json:"circuits"`
	Setups    []persistSetup    `json:"setups"`
	Jobs      []persistJob      `json:"jobs"`
	Artifacts []persistArtifact `json:"artifacts,omitempty"`
}

// envelopeWire is the decoding-only shape: circuit records arrive raw so
// their own strict decoder can require every field explicitly.
type envelopeWire struct {
	Format    int               `json:"format"`
	Circuits  []json.RawMessage `json:"circuits"`
	Setups    []persistSetup    `json:"setups"`
	Jobs      []persistJob      `json:"jobs"`
	Artifacts []persistArtifact `json:"artifacts,omitempty"`
}

func (e *envelope) UnmarshalJSON(raw []byte) error {
	// Reject repeated keys before decoding: encoding/json silently keeps the
	// last value of a duplicate. The walk is layered so a repeated field is
	// attributed to the record it belongs to (a duplicate "frozen" inside
	// circuit #3, not a generic envelope complaint).
	if err := scanEnvelopeDuplicates(raw); err != nil {
		return err
	}
	var wire envelopeWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		var se StoreError
		if errors.As(err, &se) && se.Kind == ErrDataCorrupt.Kind {
			return err // an inner record decoder already named the problem
		}
		return corruptf("data file envelope is not valid JSON: %v", err)
	}
	e.Format = wire.Format
	e.Setups = wire.Setups
	e.Jobs = wire.Jobs
	e.Artifacts = wire.Artifacts
	if wire.Circuits == nil {
		e.Circuits = nil
	} else {
		e.Circuits = make([]persistCircuit, len(wire.Circuits))
	}
	for i, craw := range wire.Circuits {
		var record persistCircuit
		if err := json.Unmarshal(craw, &record); err != nil {
			var se StoreError
			if errors.As(err, &se) {
				// Re-tag as one corruption error carrying the record index,
				// so the detail survives errors.As unwrapping in loadLocked.
				return StoreError{Kind: ErrDataCorrupt.Kind,
					Detail: fmt.Sprintf("circuit record #%d: %s", i+1, se.Detail)}
			}
			return corruptf("circuit record #%d: %v", i+1, err)
		}
		e.Circuits[i] = record
	}
	return nil
}

// envelopeTopFields are the five fields an envelope may carry. Their
// canonical (standard lowercase) spellings are the wire tags on envelope and
// envelopeWire; JSON keys are tokenized after unescaping.
var envelopeTopFields = []string{"format", "circuits", "setups", "jobs", "artifacts"}

// canonicalEnvelopeField reports the standard lowercase name of one of the
// five envelope fields when key names it under any ASCII letter-case spelling
// ("format", "FORMAT", "FoRmAt", …). Keys are already JSON-unescaped by the
// tokenizer, so an escaped spelling of any such form lands here too. A
// non-canonical key (different length or a mismatching byte, including
// non-ASCII lookalikes and unknown fields such as "CIRCUIT") returns "".
func canonicalEnvelopeField(key string) string {
	for _, field := range envelopeTopFields {
		if len(key) != len(field) {
			continue
		}
		i := 0
		for i < len(key) {
			c := key[i]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != field[i] {
				break
			}
			i++
		}
		if i == len(field) {
			return field
		}
	}
	return ""
}

// envelopeFieldLookalike reports the standard lowercase name of one of the
// five envelope fields when key names it under a spelling encoding/json's
// struct-tag matching would fold onto the field but which is not an accepted
// ASCII letter-case spelling (canonicalEnvelopeField reports those). Tag
// matching folds with the Unicode simple case-folding rules
// (strings.EqualFold), so "circuitſ" (U+017F long s, written directly or as
// the JSON escape u017f — keys arrive here already unescaped) names the
// circuits field exactly like "circuits", even standing alone. Such a
// lookalike is damage whatever its value: a later "circuitſ": [] would
// otherwise override a populated circuits array through last-value-wins and
// the records would vanish on the next commit. A key that does not fold onto
// any known field — an unrelated unknown name such as "note", or "cİrcuits"
// with U+0130 dotted capital I, which does not fold — returns "" and keeps
// the ordinary unknown-member handling.
func envelopeFieldLookalike(key string) string {
	if canonicalEnvelopeField(key) != "" {
		return "" // an accepted ASCII spelling, not a lookalike
	}
	for _, field := range envelopeTopFields {
		if strings.EqualFold(key, field) {
			return field
		}
	}
	return ""
}

// scanEnvelopeDuplicates tokenizes the committed envelope and rejects every
// repeated object key. The five known top-level fields are identified case-
// insensitively: two spellings that differ only in ASCII letter case (both
// keys already JSON-unescaped at this point) are the same field, in whatever
// order they appear and whether or not their values agree, so a later
// "CIRCUITS" or "FORMAT" can never override an earlier one through encoding/
// json's last-value-wins struct matching. Such a pair is reported as a
// duplicated top-level field named by its standard lowercase spelling.
//
// A non-ASCII spelling that folds onto a known field but is not an ASCII case
// variant ("circuitſ", "setupſ", "jobſ", "artifactſ" with U+017F long s,
// directly written or ſ-escaped) is rejected outright as data corruption:
// encoding/json would match it onto the field, so ignoring it as unknown
// could let its value replace the real one (an empty array hiding every
// record), and accepting it would let the records vanish on the next commit.
// The rule holds for the lookalike appearing alone — value a legal record, an
// empty array or null alike — and for it appearing beside the canonical
// spelling or an ASCII case variant, in either order and whether the values
// agree. The error names both the standard field and the actual spelling.
//
// Circuit, setup and job records are scanned one record at a time through
// their shared per-record readers — the record's own top-level keys are
// checked here (with the record's 1-based index, or the job's id, reported)
// by scanCircuitArray, scanSetupArray in setup_record.go and the shared job
// reader scanJobArray in job_record.go, while a nested value such as a
// constraint definition or an unknown member's object is skipped and left to
// its own strict decoder. Every other envelope member is scanned recursively
// so a duplicated key anywhere in committed data fails the read instead of
// silently resolving to its last value.
func scanEnvelopeDuplicates(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	dec.UseNumber()
	open, err := dec.Token()
	if err != nil || open != json.Delim('{') {
		return corruptf("data file envelope must be a JSON object")
	}
	// Exact spellings retain the pre-existing check for unknown keys;
	// canonical names remember a known field's first occurrence regardless
	// of the case it was written in.
	seenTop := make(map[string]bool)
	seenCanonical := make(map[string]bool)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return corruptf("data file envelope is not valid JSON: %v", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return corruptf("data file envelope is not valid JSON")
		}
		// Reject a non-ASCII spelling that encoding/json's Unicode-folded tag
		// matching would read as a known field ("circuitſ" with long s,
		// directly written or ſ-escaped) before anything else. It is damage
		// even on its own and whatever its value — a legal record, an empty
		// array or null — and whether it precedes or follows the canonical or
		// an ASCII case-variant spelling: the read may neither ignore it as an
		// unknown member nor choose one of the values and carry on.
		if canonical := envelopeFieldLookalike(key); canonical != "" {
			return corruptf("data file envelope carries top-level field %q under non-ASCII spelling %q; such a lookalike spelling is data corruption",
				canonical, key)
		}
		if canonical := canonicalEnvelopeField(key); canonical != "" {
			if seenCanonical[canonical] {
				if key == canonical {
					return corruptf("data file envelope contains duplicate top-level field %q", canonical)
				}
				return corruptf("data file envelope contains duplicate top-level field %q (also present as %q)",
					canonical, key)
			}
			seenCanonical[canonical] = true
			if canonical == "circuits" {
				if err := scanCircuitArray(dec); err != nil {
					return err
				}
				continue
			}
			if canonical == "jobs" {
				if err := scanJobArray(dec); err != nil {
					return err
				}
				continue
			}
			if canonical == "setups" {
				if err := scanSetupArray(dec); err != nil {
					return err
				}
				continue
			}
			if err := skipValueWithDupKeys(dec, "data file envelope field "+strconv.Quote(canonical)); err != nil {
				return err
			}
			continue
		}
		if seenTop[key] {
			return corruptf("data file envelope contains duplicate field %q", key)
		}
		seenTop[key] = true
		if err := skipValueWithDupKeys(dec, "data file envelope field "+strconv.Quote(key)); err != nil {
			return err
		}
	}
	if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim('}') {
		return corruptf("data file envelope is not valid JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return corruptf("data file envelope is not valid JSON: trailing data after the object")
	}
	return nil
}

// skipValueWithDupKeys consumes one JSON value positioned at its first token,
// recursively rejecting duplicate object keys.
func skipValueWithDupKeys(dec *json.Decoder, what string) error {
	tok, err := dec.Token()
	if err != nil {
		return corruptf("%s is not valid JSON: %v", what, err)
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil // scalar (including null)
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return corruptf("%s is not valid JSON: %v", what, err)
			}
			key, ok := keyTok.(string)
			if !ok {
				return corruptf("%s is not valid JSON", what)
			}
			if seen[key] {
				return corruptf("%s contains duplicate field %q", what, key)
			}
			seen[key] = true
			if err := skipValueWithDupKeys(dec, what+" "+strconv.Quote(key)); err != nil {
				return err
			}
		}
		if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim('}') {
			return corruptf("%s is not valid JSON", what)
		}
	case '[':
		index := 0
		for dec.More() {
			index++
			if err := skipValueWithDupKeys(dec, fmt.Sprintf("%s element #%d", what, index)); err != nil {
				return err
			}
		}
		if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim(']') {
			return corruptf("%s is not valid JSON", what)
		}
	}
	return nil
}

type persistTerm struct {
	Wire  int    `json:"wire"`
	Coeff string `json:"coeff"`
}

type persistConstraint struct {
	A []persistTerm `json:"a"`
	B []persistTerm `json:"b"`
	C []persistTerm `json:"c"`
}

// persistDefinition is the canonical, already-validated constraint set bound
// to one circuit version. The public/private partition is owned by the
// circuit record and is deliberately not duplicated here, so it can never
// drift from the declared counts.
type persistDefinition struct {
	Modulus     string              `json:"modulus"`
	Constraints []persistConstraint `json:"constraints"`
}

// persistArtifact is a compiled artifact: the version's identity and
// constraint count bound to its recomputable SHA-256 hash.
type persistArtifact struct {
	Name        string `json:"name"`
	Version     int    `json:"version"`
	Modulus     int64  `json:"modulus"`
	Constraints int    `json:"constraints"`
	Hash        string `json:"hash"`
}

// Store is a persistent workbench backed by one local data directory.
//
// All operations are safe for concurrent use by multiple goroutines and by
// several processes pointed at the same directory: an in-process mutex plus a
// process-shared flock serialize commits, and every commit is an atomic
// temp-file/fsync/rename replacement. Open the directory with Open and call
// Close when done.
type Store struct {
	dir  string
	mu   sync.Mutex
	lock *os.File

	data envelope
}

// resolveDataDir turns the Open argument into the absolute physical
// directory the store will be permanently pinned to.
//
// The argument is resolved exactly the way the kernel reaches it on an
// open(2) — relative components anchored to the process's current working
// directory at Open time, one component at a time, every symlink expanded at
// the position where the walk meets it. No lexical Clean is applied first:
// filepath.Abs/Clean would fold "entry/.." before the symlink was ever
// examined, so a ".." standing after a symlink pops the link's resolved
// target rather than the link's own spelling, and a/link/../x cannot be made
// to name a/x and select another data directory. Any trailing segment that
// does not exist yet (a data directory to be created by Open, or a
// not-yet-existing parent) is appended verbatim to the resolved physical
// prefix, and MkdirAll creates it beneath that prefix; a ".." after such a
// missing component cannot be walked faithfully (the kernel reports ENOENT
// there instead of folding the literal path) and fails the resolution. A
// dangling entry link — the entry itself is a symlink whose target does not
// exist — resolves through the link target's spelling and is created there.
func resolveDataDir(dir string) (string, error) {
	resolved, err := resolveExistingPrefix(dir)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(resolved) {
		// The walk itself stayed relative to the Open-time working directory;
		// only now, after every symlink has been expanded and every ".."
		// popped in walk order, anchor the result absolutely for pinning.
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return "", err
		}
	}
	return resolved, nil
}

// maxDataDirLinks bounds symlink expansion while resolving the data
// directory, matching the kernel's ELOOP guard: a link cycle must fail Open
// rather than loop forever.
const maxDataDirLinks = 255

// resolveExistingPrefix walks path the way a kernel path walk reaches it,
// accepting both absolute and Open-cwd-relative paths. Symlinks are expanded
// inline where the walk meets them and ".." pops the directory the walk is
// actually standing in (a link target, not the link spelling). Unlike
// filepath.EvalSymlinks the walk tolerates a missing tail: at the first
// component that cannot be Lstat'd, that component and the remaining literal
// components are appended to the resolved prefix so Open can create the chain
// with MkdirAll — unless the remainder contains "..", which could not be
// resolved against a non-existent entry and is an error.
func resolveExistingPrefix(path string) (string, error) {
	volLen := len(filepath.VolumeName(path))
	pathSeparator := string(os.PathSeparator)
	if volLen < len(path) && os.IsPathSeparator(path[volLen]) {
		volLen++
	}
	vol := path[:volLen] // "/" (or a drive/UNC root elsewhere); "" when relative
	dest := vol
	linksWalked := 0
	for start, end := volLen, volLen; start < len(path); start = end {
		for start < len(path) && os.IsPathSeparator(path[start]) {
			start++
		}
		end = start
		for end < len(path) && !os.IsPathSeparator(path[end]) {
			end++
		}
		if end == start {
			break
		}
		comp := path[start:end]
		if comp == "." {
			continue
		}
		if comp == ".." {
			// Pop what the walk has actually reached; symlinks have already
			// been expanded into dest, so this removes the target, never the
			// link spelling.
			r := -1
			for i := len(dest) - 1; i >= volLen; i-- {
				if os.IsPathSeparator(dest[i]) {
					r = i
					break
				}
			}
			if r < volLen || dest[r+1:] == ".." {
				if len(dest) > volLen {
					dest += pathSeparator
				}
				dest += ".."
			} else {
				dest = dest[:r]
			}
			continue
		}

		if len(dest) > len(filepath.VolumeName(dest)) && !os.IsPathSeparator(dest[len(dest)-1]) {
			dest += pathSeparator
		}
		dest += comp

		fi, err := os.Lstat(dest)
		if err != nil {
			if !os.IsNotExist(err) {
				return "", err
			}
			// Missing tail: append the remainder literally, refusing a
			// remainder containing ".." (unresolvable against this absent
			// entry), and let MkdirAll create the chain.
			for r := end; r < len(path); {
				for r < len(path) && os.IsPathSeparator(path[r]) {
					r++
				}
				s := r
				for r < len(path) && !os.IsPathSeparator(path[r]) {
					r++
				}
				switch path[s:r] {
				case "", ".":
				case "..":
					return "", fmt.Errorf("cannot resolve %q: %q does not exist and is followed by a parent reference", path, comp)
				default:
					dest += pathSeparator + path[s:r]
				}
			}
			return filepath.Clean(dest), nil
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			if !fi.Mode().IsDir() && end < len(path) {
				return "", fmt.Errorf("cannot resolve %q: %q is not a directory", path, comp)
			}
			continue
		}
		linksWalked++
		if linksWalked > maxDataDirLinks {
			return "", fmt.Errorf("cannot resolve %q: too many symlinks", path)
		}
		link, err := os.Readlink(dest)
		if err != nil {
			return "", err
		}
		// Expand the link exactly at the walk position: the still-queued
		// components (including any "..") continue from the target rather
		// than from the link spelling.
		path = link + path[end:]
		switch {
		case len(link) > 0 && os.IsPathSeparator(link[0]):
			volLen = 1
			vol = path[:1]
			dest = vol
			end = 1
		default:
			if v := filepath.VolumeName(link); v != "" {
				if len(link) > len(v) && os.IsPathSeparator(link[len(v)]) {
					v += string(os.PathSeparator)
				}
				volLen = len(v)
				vol = v
				dest = vol
				end = volLen
				break
			}
			// Relative link: it names a path from the directory holding the
			// link, which is dest without the link's own component.
			r := -1
			for i := len(dest) - 1; i >= volLen; i-- {
				if os.IsPathSeparator(dest[i]) {
					r = i
					break
				}
			}
			if r < volLen {
				dest = vol
			} else {
				dest = dest[:r]
			}
			end = 0
		}
	}
	return filepath.Clean(dest), nil
}

// Open opens (creating if needed) the workbench data in dir. If the
// directory does not exist it is created. Existing committed data is loaded
// and fully validated before Open returns; on a corrupt, truncated or
// unsupported-format file Open returns an error wrapping ErrDataCorrupt and
// leaves every file in the directory untouched.
//
// dir is resolved against the process's current working directory at Open
// time and pinned for the store's lifetime: a relative path such as
// "bench-data" names one fixed directory from the moment Open succeeds, and
// later working-directory changes never redirect the store's reads, writes,
// lock or Dir to another location.
//
// The pinning is to the actual directory opened, not to the literal path
// spelling: symlinks already present in the file system — whether dir itself
// is a link or one of its parent directories is — are resolved segment by
// segment at Open time, and Dir reports the resulting absolute physical
// directory. Repointing or removing an entry link afterwards never moves the
// store: queries, commits, the lock file and compiled-artifact bindings keep
// using the original directory even after the link changes or vanishes. A
// ".." following a symlink is resolved the way the kernel walk reaches it
// (the already-resolved link target is popped), never by lexical folding of
// the literal path, so it cannot select a different data directory. A
// not-yet-existing data directory named through a valid (possibly
// symlinked) parent is created under the parent's physical location. Paths
// to constraint definitions and input files passed to individual operations
// are not pinned this way — they keep being resolved against the caller's
// working directory at call time.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, invalidf("data directory must not be empty")
	}
	// Resolve and pin the physical location now, before anything else can
	// change. resolveDataDir walks the argument the way the kernel reaches
	// it — relative to the process working directory at Open time, symlinks
	// expanded segment by segment in walk order and ".." popped in place — so
	// a later chdir, a repointed or deleted entry link, or a ".." sitting
	// after a symlink cannot move reads, writes, the lock or Dir onto another
	// directory. The result is absolute and names the real directory opened.
	dir, err := resolveDataDir(dir)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve data directory %q: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create data directory %q: %w", dir, err)
	}
	lockPath := filepath.Join(dir, dirLockFile)
	lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("cannot open lock file in %q: %w", dir, err)
	}
	// Serialize against other processes while we read / validate.
	if err := lockExclusive(lock); err != nil {
		lock.Close()
		return nil, fmt.Errorf("cannot lock data directory %q: %w", dir, err)
	}

	s := &Store{dir: dir, lock: lock}
	if err := s.loadLocked(); err != nil {
		unlockLock(lock)
		lock.Close()
		return nil, err
	}
	// The lock is taken again per operation; holding it for the store's
	// lifetime would forbid two open handles on one directory.
	if err := unlockLock(lock); err != nil {
		lock.Close()
		return nil, fmt.Errorf("cannot release open lock on %q: %w", dir, err)
	}
	return s, nil
}

// Close releases the directory lock. It does not discard committed data.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	var first error
	if err := unlockLock(s.lock); err != nil {
		first = err
	}
	if err := s.lock.Close(); err != nil && first == nil {
		first = err
	}
	s.lock = nil
	return first
}

// Dir reports the data directory backing the store.
func (s *Store) Dir() string { return s.dir }

func (s *Store) dataPath() string { return filepath.Join(s.dir, dirDataFile) }
func (s *Store) tmpPath() string  { return filepath.Join(s.dir, dirTmpFile) }

// loadLocked reads and validates the committed file. Caller must hold the
// exclusive flock; the in-process mutex is taken by the public Open path.
func (s *Store) loadLocked() error {
	raw, err := os.ReadFile(s.dataPath())
	if errors.Is(err, os.ErrNotExist) {
		s.data = envelope{Format: FormatVersion}
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot read data file %q: %w", s.dataPath(), err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return corruptf("data file %q is empty or truncated; refusing to treat it as a new directory", s.dataPath())
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// Shape failures surfaced by the persisted-definition decoders are
		// integrity failures, not JSON syntax errors; report their detail
		// instead of the generic "not valid JSON" wording.
		var se StoreError
		if errors.As(err, &se) && se.Kind == ErrDataCorrupt.Kind {
			return corruptf("data file %q failed integrity validation: %s; original file left in place", s.dataPath(), se.Detail)
		}
		return corruptf("data file %q is not valid JSON (%v); original file left in place", s.dataPath(), err)
	}
	if env.Format == 0 {
		return corruptf("data file %q has no format version; original file left in place", s.dataPath())
	}
	if env.Format != FormatVersion {
		return corruptf("data file %q uses unsupported format version %d (this build supports %d); original file left in place",
			s.dataPath(), env.Format, FormatVersion)
	}
	if err := validateEnvelope(env); err != nil {
		return corruptf("data file %q failed integrity validation: %v; original file left in place", s.dataPath(), err)
	}
	s.data = env
	return nil
}

// commitLocked writes the pending state atomically: temp file → fsync →
// rename → fsync directory. Caller must hold both the in-process mutex and
// the exclusive flock; all in-memory mutations must already have happened and
// been rule-checked, so a failure here occurs before any success is reported.
func (s *Store) commitLocked() error {
	s.data.Format = FormatVersion
	// Stable output: records are stored sorted so files are deterministic.
	sortEnvelope(&s.data)

	payload, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode data: %w", err)
	}
	payload = append(payload, '\n')

	tmp := s.tmpPath()
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("cannot stage write in %q: %w", s.dir, err)
	}
	if _, err := f.Write(payload); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("cannot write staged data: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("cannot flush staged data: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("cannot finish staged data: %w", err)
	}
	if err := os.Rename(tmp, s.dataPath()); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("cannot commit data file: %w", err)
	}
	if err := syncDir(s.dir); err != nil {
		// The rename already happened; lack of dir fsync only weakens crash
		// durability, never correctness of what is visible now.
		return nil
	}
	return nil
}

// withLock runs fn under the in-process mutex and an exclusive flock, then
// atomically commits when fn requests it. The on-disk state is re-read after
// acquiring the lock, so concurrent commits from another process or another
// handle are always observed. Operations validate before mutating; a rejected
// operation (commit=false or an error) writes nothing and leaves committed
// state unchanged.
func (s *Store) withLock(fn func() (commit bool, err error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return invalidf("store is closed")
	}
	if err := lockExclusive(s.lock); err != nil {
		return fmt.Errorf("cannot lock data directory: %w", err)
	}
	defer unlockLock(s.lock)

	if err := s.loadLocked(); err != nil {
		return err
	}
	commit, err := fn()
	if err != nil {
		return err
	}
	if !commit {
		return nil
	}
	return s.commitLocked()
}

// withLockRead runs fn under a shared flock over freshly reloaded state.
func (s *Store) withLockRead(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return invalidf("store is closed")
	}
	if err := lockShared(s.lock); err != nil {
		return fmt.Errorf("cannot lock data directory: %w", err)
	}
	defer unlockLock(s.lock)
	if err := s.loadLocked(); err != nil {
		return err
	}
	return fn()
}

func sortEnvelope(env *envelope) {
	sort.Slice(env.Circuits, func(i, j int) bool {
		if env.Circuits[i].Name != env.Circuits[j].Name {
			return env.Circuits[i].Name < env.Circuits[j].Name
		}
		return env.Circuits[i].Version < env.Circuits[j].Version
	})
	sort.Slice(env.Setups, func(i, j int) bool {
		if env.Setups[i].Name != env.Setups[j].Name {
			return env.Setups[i].Name < env.Setups[j].Name
		}
		return env.Setups[i].Version < env.Setups[j].Version
	})
	sort.Slice(env.Jobs, func(i, j int) bool { return env.Jobs[i].ID < env.Jobs[j].ID })
	sort.Slice(env.Artifacts, func(i, j int) bool {
		if env.Artifacts[i].Name != env.Artifacts[j].Name {
			return env.Artifacts[i].Name < env.Artifacts[j].Name
		}
		return env.Artifacts[i].Version < env.Artifacts[j].Version
	})
}

// validateEnvelope re-checks every invariant on load so a tampered or
// hand-edited file cannot bypass the domain rules.
//
// A stored constraint definition is parsed and canonicalized exactly once
// per load: the circuit pass validates every defined version against its
// declared counts and keeps the resulting canonical form, and the artifact
// pass reuses that same form to verify the recorded modulus, constraint
// count and hash. The map is built fresh on each call, so the reuse never
// carries a judgment across loads — every read validates the bytes found on
// disk at that moment.
func validateEnvelope(env envelope) error {
	seenCircuit := make(map[[2]string]bool)
	circuitOK := make(map[[2]string]bool)
	canonicalDefs := make(map[[2]string]*canonicalDefinition)
	for i, c := range env.Circuits {
		key := [2]string{c.Name, itoa(c.Version)}
		if c.Name == "" || strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("circuit #%d has an empty or blank name", i+1)
		}
		if c.Version <= 0 {
			return fmt.Errorf("circuit %q version %d is not a positive integer", c.Name, c.Version)
		}
		if seenCircuit[key] {
			return fmt.Errorf("circuit %q version %d appears more than once", c.Name, c.Version)
		}
		seenCircuit[key] = true
		if c.Constraints <= 0 {
			return fmt.Errorf("circuit %q v%d: constraints must be > 0", c.Name, c.Version)
		}
		if c.PublicInputs < 0 || c.PrivateInputs < 0 {
			return fmt.Errorf("circuit %q v%d: input counts must not be negative", c.Name, c.Version)
		}
		// The same representability rule the write path enforces: wire 0 plus
		// every declared input must fit inside the platform's int. A record
		// beyond that bound can never have been written by this build and
		// cannot be checked safely, so the directory is corrupt rather than
		// silently narrowed or read with a wrapped-around layout.
		if !inputLayoutRepresentable(c.PublicInputs, c.PrivateInputs) {
			return fmt.Errorf("circuit %q v%d: input layout is not representable: 1 constant wire + %d public + %d private inputs exceeds the platform limit of %d wires",
				c.Name, c.Version, c.PublicInputs, c.PrivateInputs, maxInputWires)
		}
		if c.Definition != nil {
			def, err := validatePersistDefinition(c.Name, c.Version, c.Constraints, c.PublicInputs, c.PrivateInputs, c.Definition)
			if err != nil {
				return err
			}
			canonicalDefs[key] = def
		}
		circuitOK[key] = true
	}
	seenSetup := make(map[[2]string]bool)
	for i, p := range env.Setups {
		key := [2]string{p.Name, itoa(p.Version)}
		if !circuitOK[key] {
			return fmt.Errorf("setup #%d belongs to unknown circuit %q v%d", i+1, p.Name, p.Version)
		}
		c := findPersistCircuit(env.Circuits, p.Name, p.Version)
		if c == nil || !c.Frozen {
			return fmt.Errorf("setup belongs to non-frozen circuit %q v%d", p.Name, p.Version)
		}
		if seenSetup[key] {
			return fmt.Errorf("setup for %q v%d appears more than once", p.Name, p.Version)
		}
		seenSetup[key] = true
	}
	seenJob := make(map[string]bool)
	for i, j := range env.Jobs {
		if j.ID == "" || strings.TrimSpace(j.ID) == "" {
			return fmt.Errorf("job #%d has an empty or blank id", i+1)
		}
		if seenJob[j.ID] {
			return fmt.Errorf("job %q appears more than once", j.ID)
		}
		seenJob[j.ID] = true
		if j.Kind != "prove" {
			return fmt.Errorf("job %q has unsupported kind %q", j.ID, j.Kind)
		}
		if j.Attempt <= 0 {
			return fmt.Errorf("job %q: attempt must be a positive integer", j.ID)
		}
		key := [2]string{j.Circuit, itoa(j.Version)}
		if !circuitOK[key] {
			return fmt.Errorf("job %q binds to unknown circuit %q v%d", j.ID, j.Circuit, j.Version)
		}
		c := findPersistCircuit(env.Circuits, j.Circuit, j.Version)
		if c == nil || !c.Frozen {
			return fmt.Errorf("job %q binds to non-frozen circuit %q v%d", j.ID, j.Circuit, j.Version)
		}
		if !seenSetup[key] {
			return fmt.Errorf("job %q binds to circuit %q v%d without a trusted setup", j.ID, j.Circuit, j.Version)
		}
		// A recorded compiled-artifact binding must still resolve against the
		// pinned version's artifact; a dangling or mismatched binding makes
		// the file unreadable rather than silently dropping the binding.
		if j.CompiledHash != "" {
			artifact := findArtifact(env.Artifacts, j.Circuit, j.Version)
			if artifact == nil {
				return fmt.Errorf("job %q binds to compiled artifact of %q v%d which is missing", j.ID, j.Circuit, j.Version)
			}
			if artifact.Hash != j.CompiledHash {
				return fmt.Errorf("job %q compiled hash %q does not match the artifact of %q v%d (hash %q)",
					j.ID, j.CompiledHash, j.Circuit, j.Version, artifact.Hash)
			}
		}
	}
	seenArtifact := make(map[[2]string]bool)
	for i, a := range env.Artifacts {
		key := [2]string{a.Name, itoa(a.Version)}
		if !circuitOK[key] {
			return fmt.Errorf("artifact #%d belongs to unknown circuit %q v%d", i+1, a.Name, a.Version)
		}
		c := findPersistCircuit(env.Circuits, a.Name, a.Version)
		if c == nil || !c.Frozen {
			return fmt.Errorf("artifact belongs to non-frozen circuit %q v%d", a.Name, a.Version)
		}
		if seenArtifact[key] {
			return fmt.Errorf("artifact for %q v%d appears more than once", a.Name, a.Version)
		}
		seenArtifact[key] = true
		if c.Definition == nil {
			return fmt.Errorf("artifact for %q v%d has no constraint definition", a.Name, a.Version)
		}
		if a.Constraints != c.Constraints {
			return fmt.Errorf("artifact for %q v%d constraint count %d disagrees with the version's %d",
				a.Name, a.Version, a.Constraints, c.Constraints)
		}
		// The circuit pass above already validated and canonicalized this
		// version's definition (every artifact requires a defined version,
		// checked just above), so the artifact check reuses that one result
		// instead of re-parsing and re-normalizing the same stored definition.
		def := canonicalDefs[key]
		if int64(a.Modulus) != def.modulus || len(def.constraints) != a.Constraints {
			return fmt.Errorf("artifact for %q v%d is inconsistent with its stored definition", a.Name, a.Version)
		}
		wantHash := artifactHash(a.Name, a.Version, def)
		if a.Hash != wantHash {
			return fmt.Errorf("artifact for %q v%d hash %q does not recompute from the definition (want %q)",
				a.Name, a.Version, a.Hash, wantHash)
		}
	}
	return nil
}

// validatePersistDefinition re-validates a stored definition against its
// version's declared counts: prime modulus, in-range wires and an exact
// constraint count match. On success it returns the canonical form built
// during validation, so a later check against the same version's artifact
// reuses it instead of parsing and normalizing the definition a second time.
func validatePersistDefinition(name string, version, constraints, public, private int, def *persistDefinition) (*canonicalDefinition, error) {
	parsed, err := definitionFromPersist(*def, public, private)
	if err != nil {
		return nil, fmt.Errorf("circuit %q v%d has a corrupt constraint definition: %w", name, version, err)
	}
	if !parsed.compatibleWith(constraints, public, private) {
		return nil, fmt.Errorf("circuit %q v%d definition is incompatible with its declared counts", name, version)
	}
	return parsed, nil
}

func findPersistCircuit(cs []persistCircuit, name string, version int) *persistCircuit {
	for i := range cs {
		if cs[i].Name == name && cs[i].Version == version {
			return &cs[i]
		}
	}
	return nil
}

func itoa(v int) string { return fmt.Sprintf("%d", v) }
