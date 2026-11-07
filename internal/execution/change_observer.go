package execution

import (
	"container/list"
	"crypto/sha256"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxObservedFiles    = 20_000
	maxHashedFileBytes  = 1 << 20
	maxTotalHashedBytes = 32 << 20
	maxHashCacheEntries = 32_000
)

type ChangeKind string

const (
	ChangeCreated  ChangeKind = "created"
	ChangeModified ChangeKind = "modified"
	ChangeDeleted  ChangeKind = "deleted"
)

type Change struct {
	Path       string     `json:"path"`
	Kind       ChangeKind `json:"kind"`
	Aggregated bool       `json:"aggregated,omitempty"`
	Count      int        `json:"count,omitempty"`
}

type fileFingerprint struct {
	Mode    fs.FileMode
	Size    int64
	ModTime int64
	Digest  [sha256.Size]byte
	Hashed  bool
	// Aggregated marks a bounded fingerprint for a generated directory. The
	// observer never enumerates that tree into individual changes.
	Aggregated bool
	Count      int
}

// hashCacheKey identifies file content by path plus the stat fields we treat
// as change signals. A file rewritten with identical size and mtime within the
// cache window can be missed; the mtime+size tuple is the same heuristic
// editors, make, and watchexec rely on.
type hashCacheKey struct {
	Path    string
	Size    int64
	ModTime int64
}

type hashCacheEntry struct {
	key     hashCacheKey
	digest  [sha256.Size]byte
	expiry  time.Time
	element *list.Element
}

// hashCache memoises SHA-256 digests across observer snapshots. Change
// observers run before and after every command, and most workspaces are
// unchanged between runs, so re-hashing every file each time dominates CPU.
// The cache is LRU with a TTL; it is only an optimisation, so any miss or
// eviction is safe.
type hashCache struct {
	mu       sync.Mutex
	entries  map[hashCacheKey]*hashCacheEntry
	order    *list.List
	capacity int
	ttl      time.Duration
	now      func() time.Time
}

var observedHashCache = newHashCache(maxHashCacheEntries, 10*time.Minute)

func newHashCache(capacity int, ttl time.Duration) *hashCache {
	return &hashCache{
		entries:  make(map[hashCacheKey]*hashCacheEntry, capacity),
		order:    list.New(),
		capacity: capacity,
		ttl:      ttl,
		now:      time.Now,
	}
}

func (cache *hashCache) get(key hashCacheKey) ([sha256.Size]byte, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[key]
	if !ok {
		return [sha256.Size]byte{}, false
	}
	if !cache.now().Before(entry.expiry) {
		cache.remove(entry)
		return [sha256.Size]byte{}, false
	}
	cache.order.MoveToFront(entry.element)
	return entry.digest, true
}

func (cache *hashCache) put(key hashCacheKey, digest [sha256.Size]byte) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if entry, ok := cache.entries[key]; ok {
		entry.digest = digest
		entry.expiry = cache.now().Add(cache.ttl)
		cache.order.MoveToFront(entry.element)
		return
	}
	entry := &hashCacheEntry{key: key, digest: digest, expiry: cache.now().Add(cache.ttl)}
	entry.element = cache.order.PushFront(entry)
	cache.entries[key] = entry
	for cache.order.Len() > cache.capacity {
		oldest := cache.order.Back()
		if oldest == nil {
			break
		}
		cache.remove(oldest.Value.(*hashCacheEntry))
	}
}

func (cache *hashCache) remove(entry *hashCacheEntry) {
	cache.order.Remove(entry.element)
	delete(cache.entries, entry.key)
}

// ChangeObserver records a bounded workspace snapshot around a command. It
// deliberately skips Zero/repository control metadata and large generated
// dependency trees; those paths must neither be read as command evidence nor
// flood the Files panel.
type ChangeObserver struct {
	root   string
	before map[string]fileFingerprint
	valid  bool
}

func NewChangeObserver(root string) *ChangeObserver {
	root = filepath.Clean(strings.TrimSpace(root))
	before, ok := snapshotWorkspace(root)
	return &ChangeObserver{root: root, before: before, valid: ok}
}

func (observer *ChangeObserver) Changes() []Change {
	if observer == nil || !observer.valid {
		return nil
	}
	after, ok := snapshotWorkspace(observer.root)
	if !ok {
		return nil
	}
	changes := make([]Change, 0)
	for path, current := range after {
		previous, existed := observer.before[path]
		if !existed {
			changes = append(changes, changeFromFingerprint(path, ChangeCreated, current))
			continue
		}
		if previous != current {
			changes = append(changes, changeFromFingerprint(path, ChangeModified, current))
		}
	}
	for path, previous := range observer.before {
		if _, exists := after[path]; !exists {
			changes = append(changes, changeFromFingerprint(path, ChangeDeleted, previous))
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
}

func snapshotWorkspace(root string) (map[string]fileFingerprint, bool) {
	if root == "" || root == "." {
		return nil, false
	}
	files := make(map[string]fileFingerprint)
	hashedBytes := int64(0)
	ok := true
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil
		}
		if entry.IsDir() {
			if protectedObservationDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			if generatedObservationDirectory(entry.Name()) {
				files[filepath.ToSlash(relative)+"/"] = generatedTreeFingerprint(path)
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if len(files) >= maxObservedFiles {
			ok = false
			return fs.SkipAll
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		fingerprint := fileFingerprint{Mode: info.Mode(), Size: info.Size(), ModTime: info.ModTime().UnixNano()}
		if info.Size() <= maxHashedFileBytes {
			key := hashCacheKey{Path: path, Size: info.Size(), ModTime: fingerprint.ModTime}
			if digest, ok := observedHashCache.get(key); ok {
				fingerprint.Digest = digest
				fingerprint.Hashed = true
			} else if hashedBytes+info.Size() <= maxTotalHashedBytes {
				if digest, err := hashObservedFile(path); err == nil {
					fingerprint.Digest = digest
					fingerprint.Hashed = true
					hashedBytes += info.Size()
					observedHashCache.put(key, digest)
				}
			}
		}
		files[filepath.ToSlash(relative)] = fingerprint
		return nil
	})
	if err != nil {
		return nil, false
	}
	return files, ok
}

func protectedObservationDirectory(name string) bool {
	switch name {
	case ".git", ".zero", ".agents":
		return true
	default:
		return false
	}
}

func generatedObservationDirectory(name string) bool {
	switch name {
	case "node_modules", ".pnpm-store", ".yarn", "dist", "coverage", ".cache":
		return true
	default:
		return false
	}
}

// generatedTreeFingerprint samples only direct children and caps the work. It
// is intentionally qualitative: the UI needs to say that a generated tree
// changed, not inventory dependency contents.
func generatedTreeFingerprint(root string) fileFingerprint {
	directory, err := os.Open(root)
	if err != nil {
		return fileFingerprint{Aggregated: true, Count: -1}
	}
	defer directory.Close()
	const maxEntries = 256
	entries, err := directory.ReadDir(maxEntries + 1)
	if err != nil && err != io.EOF {
		return fileFingerprint{Aggregated: true, Count: -1}
	}
	hash := sha256.New()
	count := len(entries)
	if count > maxEntries {
		entries = entries[:maxEntries]
		count = -1
	}
	for _, entry := range entries {
		_, _ = io.WriteString(hash, entry.Name())
		if info, infoErr := entry.Info(); infoErr == nil {
			_, _ = io.WriteString(hash, info.Mode().String())
			_, _ = io.WriteString(hash, strconv.FormatInt(info.ModTime().UnixNano(), 10))
		}
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return fileFingerprint{Digest: digest, Hashed: true, Aggregated: true, Count: count}
}

func changeFromFingerprint(path string, kind ChangeKind, fingerprint fileFingerprint) Change {
	return Change{Path: path, Kind: kind, Aggregated: fingerprint.Aggregated, Count: fingerprint.Count}
}

func hashObservedFile(path string) ([sha256.Size]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return [sha256.Size]byte{}, err
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}
