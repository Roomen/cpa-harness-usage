package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func syntheticBundle() ([]byte, []byte) {
	payload := []byte("synthetic executable payload")
	digest := sha256.Sum256(payload)
	executable := "ccusage"
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	manifest, _ := json.Marshal(collectorManifest{
		Version: "1.2.3", GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		Executable: executable, SHA256: hex.EncodeToString(digest[:]),
	})
	return payload, manifest
}

func TestCollectorCacheReuseAndRepair(t *testing.T) {
	cache := t.TempDir()
	payload, manifest := syntheticBundle()
	path, err := materializeCollector(cache, payload, manifest)
	if err != nil {
		t.Fatal("materialize synthetic collector")
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal("stat cached collector")
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			reused, err := materializeCollector(cache, payload, manifest)
			if err != nil || reused != path {
				t.Error("concurrent cache reuse failed")
			}
		})
	}
	workers.Wait()
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("cache hit replaced the existing executable")
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0700); err != nil {
		t.Fatal("write synthetic corruption")
	}
	if _, err := materializeCollector(cache, payload, manifest); err != nil {
		t.Fatal("repair corrupted cached executable")
	}
	restored, err := os.ReadFile(path)
	if err != nil || string(restored) != string(payload) {
		t.Fatal("cached executable was not restored")
	}
}

func TestCollectorRejectsInvalidBundleAndSymlinks(t *testing.T) {
	payload, raw := syntheticBundle()
	var manifest collectorManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*collectorManifest){
		func(manifest *collectorManifest) { manifest.SHA256 = "invalid" },
		func(manifest *collectorManifest) { manifest.GOOS = "unsupported" },
		func(manifest *collectorManifest) { manifest.GOARCH = "unsupported" },
		func(manifest *collectorManifest) { manifest.Executable = "../outside" },
	} {
		changed := manifest
		mutate(&changed)
		invalid, _ := json.Marshal(changed)
		if _, err := materializeCollector(t.TempDir(), payload, invalid); err == nil {
			t.Fatal("invalid bundle accepted")
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	cache := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(cache, "cpa-harness-usage")); err != nil {
		t.Fatal("create synthetic cache symlink")
	}
	if _, err := materializeCollector(cache, payload, raw); err == nil {
		t.Fatal("symlink cache directory accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("symlink target was modified")
	}
}
