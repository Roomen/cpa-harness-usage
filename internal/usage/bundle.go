package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

const DefaultBinary = "bundled"

var (
	errCollectorUnavailable = errors.New("collector unavailable")
	bundleMu                sync.Mutex
)

type collectorManifest struct {
	Version    string `json:"version"`
	GOOS       string `json:"goos"`
	GOARCH     string `json:"goarch"`
	Executable string `json:"executable"`
	SHA256     string `json:"sha256"`
}

func bundledBinary() (string, error) {
	payload, raw, err := embeddedCollector()
	if err != nil {
		return "", errCollectorUnavailable
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", errCollectorUnavailable
	}
	return materializeCollector(cache, payload, raw)
}

func materializeCollector(cache string, payload, raw []byte) (string, error) {
	bundleMu.Lock()
	defer bundleMu.Unlock()

	var manifest collectorManifest
	digest := sha256.Sum256(payload)
	executable := "ccusage"
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	if json.Unmarshal(raw, &manifest) != nil || len(payload) == 0 || manifest.Version == "" ||
		manifest.GOOS != runtime.GOOS || manifest.GOARCH != runtime.GOARCH ||
		manifest.Executable != executable || manifest.SHA256 != hex.EncodeToString(digest[:]) {
		return "", errCollectorUnavailable
	}
	if err := os.MkdirAll(cache, 0700); err != nil {
		return "", errCollectorUnavailable
	}
	directory := cache
	for _, component := range []string{"cpa-harness-usage", "collectors", runtime.GOOS + "-" + runtime.GOARCH, manifest.SHA256} {
		directory = filepath.Join(directory, component)
		if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", errCollectorUnavailable
		}
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
			return "", errCollectorUnavailable
		}
	}
	target := filepath.Join(directory, executable)
	if validCachedCollector(target, digest, len(payload)) {
		return target, nil
	}
	if info, err := os.Lstat(target); err == nil && !info.Mode().IsRegular() {
		return "", errCollectorUnavailable
	}
	temporary, err := os.CreateTemp(directory, ".collector-*")
	if err != nil {
		return "", errCollectorUnavailable
	}
	defer os.Remove(temporary.Name())
	_, writeErr := temporary.Write(payload)
	syncErr := temporary.Sync()
	modeErr := temporary.Chmod(0700)
	closeErr := temporary.Close()
	if writeErr != nil || syncErr != nil || modeErr != nil || closeErr != nil {
		return "", errCollectorUnavailable
	}
	if err := os.Rename(temporary.Name(), target); err != nil && !validCachedCollector(target, digest, len(payload)) {
		return "", errCollectorUnavailable
	}
	return target, nil
}

func validCachedCollector(path string, digest [sha256.Size]byte, size int) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(size) ||
		(runtime.GOOS != "windows" && info.Mode().Perm() != 0700) {
		return false
	}
	payload, err := os.ReadFile(path)
	return err == nil && sha256.Sum256(payload) == digest
}
