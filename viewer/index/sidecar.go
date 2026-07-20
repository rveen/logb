package index

import (
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// sidecarVersion is bumped whenever the cached shape changes. A mismatch
// discards the cache rather than trying to migrate it: rebuilding is a scan,
// which is exactly what the cache is an optimisation for, and a subtly
// mis-migrated index would produce wrong charts rather than a slow open.
// 3: Tier 1 gained presence counts for event fields, which earlier caches do
// not carry. An old cache would give an event lane a density of zero
// everywhere — a wrong chart, not a slow one, which is precisely the case this
// constant exists for.
const sidecarVersion = 3

// sidecarMagic guards against a file of the same name that is not ours.
const sidecarMagic = "logbview-index"

// hashPrefixBytes is at most how much of the source is fingerprinted.
//
// Deliberately not the whole file: the point of the cache is to avoid reading a
// gigabyte, so hashing a gigabyte to validate it would defeat it. The prefix
// covers the file header and the first segment's schemas, which is what
// actually has to still be true for the cached offsets to mean anything;
// size and mtime catch the rest.
//
// How many bytes were actually hashed is recorded alongside the hash, because a
// short file grows into this limit. Hashing "the first 4 KB" of a 500-byte file
// and then of its 8 KB successor compares two different quantities and can
// never match, which would silently disable extension for exactly the small
// live-logger files it exists for.
const hashPrefixBytes = 4096

// Sidecar is the cached form of an indexed file.
//
// It holds Tier 0 and Tier 1 — where every frame is and what each holds — but
// not the schemas. Schemas are recovered by replaying the SCHEMA frames the
// index points at, which costs a handful of small reads and takes them from the
// file rather than from a copy that could have drifted.
type Sidecar struct {
	Magic   string
	Version int

	// Identity of the source at the time of indexing.
	SourceSize int64
	SourceMod  int64 // unix nanoseconds
	SourceHash string
	HashLen    int64 // bytes covered by SourceHash
	IndexedAt  int64
	IndexedIn  int64 // nanoseconds the scan took, for reporting

	Epoch       int64
	HasEpoch    bool
	Truncated   bool
	Closed      bool
	Unsupported []string
	Meta        []MetaKV
	Attachments []Attachment

	Segments []*Segment
	Data     []DataFrame

	// Stats is keyed by stream UUID (hex) and indexed [frame ordinal][field].
	Stats map[string][][]Stat
}

// MetaKV is a metadata pair. logb.Meta has unexported-free fields but lives in
// the core package; copying it keeps the cached form independent of it.
type MetaKV struct{ Key, Value string }

// SidecarPaths returns where the cache for a source file may live, in
// preference order.
//
// Beside the file first, which keeps the cache with the data it describes and
// makes it obvious what to delete. Read-only media is a first-class case for
// this format — a log pulled off an SD card is exactly the thing someone opens
// a viewer on — so the user cache directory, keyed by the source's absolute
// path, is the fallback.
//
// Deliberately side-effect free. An earlier version probed writability by
// opening the beside path with O_CREATE, which left an empty file next to every
// file ever opened and made the first load report a spurious decode failure.
// Writability is discovered by trying to save, which is the only moment it
// actually matters.
func SidecarPaths(source string) []string {
	paths := []string{source + ".logbview"}

	abs, err := filepath.Abs(source)
	if err != nil {
		abs = source
	}
	sum := sha256.Sum256([]byte(abs))
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return append(paths, filepath.Join(dir, "logbview", hex.EncodeToString(sum[:16])+".logbview"))
}

// fingerprint identifies a source file cheaply.
//
// want caps how many leading bytes are hashed; pass 0 for the default. Passing
// the length an earlier fingerprint covered is what makes a file that has since
// grown comparable with its earlier self.
func fingerprint(path string, want int64) (size, mod, hashLen int64, hash string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, "", err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return 0, 0, 0, "", err
	}

	n := int64(hashPrefixBytes)
	if want > 0 && want < n {
		n = want
	}
	if st.Size() < n {
		n = st.Size()
	}

	buf := make([]byte, n)
	got, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return 0, 0, 0, "", err
	}
	sum := sha256.Sum256(buf[:got])
	return st.Size(), st.ModTime().UnixNano(), int64(got), hex.EncodeToString(sum[:]), nil
}

// SaveSidecar writes the cache for an indexed file.
//
// A failure here is never fatal to the caller: the cache is an accelerator, and
// a viewer that refuses to open a file because it could not write beside it
// would be worse than a slow one.
func SaveSidecar(fi *File, took time.Duration) error {
	size, mod, hashLen, hash, err := fingerprint(fi.Path, 0)
	if err != nil {
		return err
	}

	sc := &Sidecar{
		Magic:       sidecarMagic,
		Version:     sidecarVersion,
		SourceSize:  size,
		SourceMod:   mod,
		SourceHash:  hash,
		HashLen:     hashLen,
		IndexedAt:   time.Now().UnixNano(),
		IndexedIn:   int64(took),
		Epoch:       fi.Epoch,
		HasEpoch:    fi.HasEpoch,
		Truncated:   fi.Truncated,
		Closed:      fi.Closed,
		Unsupported: fi.Unsupported,
		Attachments: fi.Attachments,
		Segments:    fi.Frames.Segments,
		Data:        fi.Frames.Data,
		Stats:       map[string][][]Stat{},
	}
	for _, m := range fi.Meta {
		sc.Meta = append(sc.Meta, MetaKV{m.Key, m.Value})
	}
	for _, st := range fi.Streams {
		sc.Stats[st.UUID] = st.stats
	}

	var lastErr error
	for _, path := range SidecarPaths(fi.Path) {
		if err := writeSidecar(path, sc); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

// writeSidecar encodes to path via a temporary file and a rename, so an
// interrupted save leaves the previous cache intact rather than a half-written
// one that would then have to be detected.
func writeSidecar(path string, sc *Sidecar) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".logbview-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if err := gob.NewEncoder(tmp).Encode(sc); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ErrSidecarStale means the cache does not describe the file as it is now.
var ErrSidecarStale = errors.New("index: sidecar does not match the source")

// LoadSidecar reads and validates the cache for a source file.
//
// Returns an error wrapping fs.ErrNotExist when there simply is no cache, which
// is the ordinary first-open case and not worth reporting to anyone.
func LoadSidecar(source string) (*Sidecar, error) {
	var sc Sidecar
	found := false
	for _, path := range SidecarPaths(source) {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		err = gob.NewDecoder(f).Decode(&sc)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		found = true
		break
	}
	if !found {
		return nil, fmt.Errorf("no sidecar for %s: %w", source, fs.ErrNotExist)
	}
	if sc.Magic != sidecarMagic {
		return nil, fmt.Errorf("%w: not a sidecar", ErrSidecarStale)
	}
	if sc.Version != sidecarVersion {
		return nil, fmt.Errorf("%w: version %d, want %d", ErrSidecarStale, sc.Version, sidecarVersion)
	}

	// Hash exactly the span the cache covered, so a file that has since grown
	// past hashPrefixBytes still compares like for like.
	size, mod, hashLen, hash, err := fingerprint(source, sc.HashLen)
	if err != nil {
		return nil, err
	}
	if hashLen != sc.HashLen || hash != sc.SourceHash {
		// The file's opening bytes changed, so it is a different recording
		// whatever its name says.
		return nil, fmt.Errorf("%w: prefix hash differs over %d bytes (cache covered %d)",
			ErrSidecarStale, hashLen, sc.HashLen)
	}
	if size < sc.SourceSize {
		// It shrank. Nothing in this format shrinks; treat it as unrelated.
		return nil, fmt.Errorf("%w: shrank from %d to %d", ErrSidecarStale, sc.SourceSize, size)
	}
	if size == sc.SourceSize && mod != sc.SourceMod {
		// Same length, different mtime: something rewrote it in place, which
		// an append-only format should never do. Do not trust the offsets.
		return nil, fmt.Errorf("%w: rewritten in place", ErrSidecarStale)
	}
	return &sc, nil
}

// Grown reports how many bytes were appended since the cache was written.
//
// A Logb file legitimately grows: a logger appends, and someone opens the file
// while it is still being written. Because nothing in the format points forward
// and every segment restates its schemas, the already-indexed prefix stays
// valid and only the tail needs scanning.
func (sc *Sidecar) Grown(source string) (int64, error) {
	size, _, _, _, err := fingerprint(source, sc.HashLen)
	if err != nil {
		return 0, err
	}
	return size - sc.SourceSize, nil
}

// restore rebuilds a File from the cache, recovering schemas from the source.
func (sc *Sidecar) restore(path string, size int64) (*File, error) {
	fi := &File{
		Path:        path,
		Size:        size,
		Epoch:       sc.Epoch,
		HasEpoch:    sc.HasEpoch,
		Truncated:   sc.Truncated,
		Closed:      sc.Closed,
		Unsupported: sc.Unsupported,
		Attachments: sc.Attachments,
		Frames:      &FrameIndex{Segments: sc.Segments, Data: sc.Data},
	}
	for _, m := range sc.Meta {
		fi.Meta = append(fi.Meta, metaOf(m))
	}

	acc, err := NewAccessor(path, fi.Frames)
	if err != nil {
		return nil, err
	}
	defer acc.Close()

	// Schemas come from the file, never from the cache. Walking every segment
	// rather than only the first is what picks up a stream that appears
	// partway through a recording.
	byUUID := map[string]*Stream{}
	for i := range fi.Frames.Segments {
		schemas, err := acc.Schemas(i)
		if err != nil {
			return nil, fmt.Errorf("index: recovering schemas for segment %d: %w", i, err)
		}
		for _, s := range schemas {
			key := hex.EncodeToString(s.UUID[:])
			if byUUID[key] != nil {
				continue
			}
			st := newStream(s)
			byUUID[key] = st
			fi.Streams = append(fi.Streams, st)
		}
	}

	for _, d := range fi.Frames.Data {
		key := hex.EncodeToString(d.UUID[:])
		st := byUUID[key]
		if st == nil {
			return nil, fmt.Errorf("index: cached frame references unknown stream %s", key)
		}
		st.FrameList = append(st.FrameList, d)
		st.Records += int(d.Count)
		st.noteRun(d.RunID, nil)
	}
	for _, st := range fi.Streams {
		st.stats = sc.Stats[st.UUID]
		if len(st.stats) != len(st.FrameList) {
			return nil, fmt.Errorf("index: cached stream %q has %d frames but %d stat rows",
				st.Name, len(st.FrameList), len(st.stats))
		}
		st.span()
	}
	sortFile(fi)
	return fi, nil
}
