package execution

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestChangeObserverReportsCreateModifyAndDelete(t *testing.T) {
	root := t.TempDir()
	modified := filepath.Join(root, "modified.txt")
	deleted := filepath.Join(root, "deleted.txt")
	if err := os.WriteFile(modified, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deleted, []byte("delete"), 0o644); err != nil {
		t.Fatal(err)
	}
	observer := NewChangeObserver(root)
	if err := os.WriteFile(modified, []byte("after!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "created.ts"), []byte("export {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := []Change{
		{Path: "deleted.txt", Kind: ChangeDeleted},
		{Path: "modified.txt", Kind: ChangeModified},
		{Path: "src/created.ts", Kind: ChangeCreated},
	}
	if got := observer.Changes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Changes() = %#v, want %#v", got, want)
	}
}

func TestChangeObserverSkipsControlMetadataAndSummarizesGeneratedTrees(t *testing.T) {
	root := t.TempDir()
	observer := NewChangeObserver(root)
	for _, path := range []string{
		filepath.Join(root, ".zero", "config.json"),
		filepath.Join(root, ".agents", "skills", "x"),
		filepath.Join(root, ".git", "config"),
		filepath.Join(root, "node_modules", "pkg", "index.js"),
		filepath.Join(root, "dist", "bundle.js"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("generated"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := []Change{
		{Path: "dist/", Kind: ChangeCreated, Aggregated: true, Count: 1},
		{Path: "node_modules/", Kind: ChangeCreated, Aggregated: true, Count: 1},
	}
	if got := observer.Changes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Changes() = %#v, want generated summaries without control metadata %#v", got, want)
	}
}

func TestChangeObserverDetectsSameSizeModificationWithHashCache(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "app.go")
	if err := os.WriteFile(path, []byte("aaaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	observer := NewChangeObserver(root)
	if err := os.WriteFile(path, []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	want := []Change{{Path: "app.go", Kind: ChangeModified}}
	if got := observer.Changes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Changes() = %#v, want %#v", got, want)
	}
}

func TestHashCacheReturnsDigestUntilTTLExpiry(t *testing.T) {
	cache := newHashCache(8, time.Minute)
	now := time.Unix(1_700_000_000, 0)
	cache.now = func() time.Time { return now }
	key := hashCacheKey{Path: "app.go", Size: 4, ModTime: int64(now.UnixNano())}
	digest := sha256.Sum256([]byte("aaaa"))

	if _, ok := cache.get(key); ok {
		t.Fatal("get() on empty cache reported a hit")
	}
	cache.put(key, digest)
	if got, ok := cache.get(key); !ok || got != digest {
		t.Fatalf("get() = %x, %t; want cached digest", got, ok)
	}

	now = now.Add(2 * time.Minute)
	if _, ok := cache.get(key); ok {
		t.Fatal("get() returned a hit after TTL expiry")
	}

	// Re-put after expiry refreshes the entry.
	cache.put(key, digest)
	if got, ok := cache.get(key); !ok || got != digest {
		t.Fatalf("get() = %x, %t; want refreshed digest", got, ok)
	}
}

func TestHashCacheEvictsLeastRecentlyUsed(t *testing.T) {
	cache := newHashCache(2, time.Hour)
	keys := []hashCacheKey{
		{Path: "a", Size: 1},
		{Path: "b", Size: 1},
		{Path: "c", Size: 1},
	}
	for i, key := range keys[:2] {
		cache.put(key, sha256.Sum256([]byte{byte(i)}))
	}
	// Touch "a" so "b" becomes the least recently used entry.
	if _, ok := cache.get(keys[0]); !ok {
		t.Fatal("expected hit for key a")
	}
	cache.put(keys[2], sha256.Sum256([]byte{2}))

	if _, ok := cache.get(keys[1]); ok {
		t.Fatal("expected least-recently-used key b to be evicted")
	}
	if _, ok := cache.get(keys[0]); !ok {
		t.Fatal("expected recently-used key a to survive eviction")
	}
	if _, ok := cache.get(keys[2]); !ok {
		t.Fatal("expected newly inserted key c to be present")
	}
}
