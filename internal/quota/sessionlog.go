package quota

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Codex session-log fallback.
//
// Verified against openai/codex rust-v0.163.0: rollout files live under
// $CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl; each line is
// {"timestamp":…,"type":"event_msg","payload":{"type":"token_count",
// "rate_limits":RateLimitSnapshot}} (codex-rs/history RolloutLine and
// RolloutItem, protocol EventMsg::TokenCount, persisted by rollout policy).
// RateLimitSnapshot.primary/secondary are RateLimitWindow{used_percent f64,
// window_minutes i64?, resets_at i64? (Unix seconds)}. rust-v0.46.0 used
// resets_in_seconds (relative to the line) instead of resets_at; it is still
// accepted. Files older than seven days may be zstd-compressed (.jsonl.zst);
// they are skipped because their windows have already reset.

const (
	sessionLookbackDays = 8
	sessionMaxFiles     = 6
	sessionTailBytes    = 512 << 10
	sessionMaxDirFiles  = 4096
	// sessionMaxDirs bounds how many directories one scan reads. Directories
	// are visited newest first, so the budget covers roughly the most recent
	// eleven years of daily session directories before it is exhausted.
	sessionMaxDirs = 4096
)

type sessionObservation struct {
	observedAt time.Time
	plan       string
	windows    []Window
}

type sessionReader func(now time.Time) (sessionObservation, bool)

// newSessionReader reads $CODEX_HOME/sessions (or ~/.codex/sessions).
func newSessionReader(creds *fileCredentials) sessionReader {
	return func(now time.Time) (sessionObservation, bool) {
		home := creds.codexHome()
		if home == "" {
			return sessionObservation{}, false
		}
		return latestSessionRateLimits(filepath.Join(home, "sessions"), now)
	}
}

// latestSessionRateLimits returns the newest token_count rate-limit snapshot
// among the most recently modified rollout files, whatever directory they are
// in; an observation older than sessionLookbackDays is treated as absent.
func latestSessionRateLimits(root string, now time.Time) (sessionObservation, bool) {
	files := recentRolloutFiles(root)
	var best sessionObservation
	found := false
	for _, path := range files {
		obs, ok := lastRateLimitsInFile(path, now)
		if ok && (!found || obs.observedAt.After(best.observedAt)) {
			best, found = obs, true
		}
	}
	if !found || now.Sub(best.observedAt) > sessionLookbackDays*24*time.Hour {
		return sessionObservation{}, false
	}
	return best, true
}

type rolloutFile struct {
	path    string
	modTime time.Time
}

// recentRolloutFiles returns rollout files ordered by modification time. The
// session tree groups files by creation date, so a resumed old session keeps
// being appended in its original YYYY/MM/DD directory; selecting directories
// by date would miss it. The walk therefore visits directories newest first
// and chooses files by modification time, bounded by sessionMaxDirs.
func recentRolloutFiles(root string) []string {
	var files []rolloutFile
	dirs := 0
	var walk func(dir string, depth int) bool
	walk = func(dir string, depth int) bool {
		if dirs >= sessionMaxDirs {
			return false
		}
		dirs++
		entries, err := os.ReadDir(dir)
		if err != nil {
			return true
		}
		if depth == 3 {
			matched := 0
			for _, entry := range entries {
				if !entry.IsDir() && isRolloutFile(entry.Name()) {
					matched++
				}
			}
			skip := matched - sessionMaxDirFiles
			for _, entry := range entries {
				name := entry.Name()
				if entry.IsDir() || !isRolloutFile(name) {
					continue
				}
				if skip > 0 {
					skip--
					continue
				}
				info, err := entry.Info()
				if err != nil || !info.Mode().IsRegular() {
					continue
				}
				files = append(files, rolloutFile{filepath.Join(dir, name), info.ModTime()})
			}
			return true
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
		for _, entry := range entries {
			if !entry.IsDir() || !numericDir(entry.Name(), depth) {
				continue
			}
			if !walk(filepath.Join(dir, entry.Name()), depth+1) {
				return false
			}
		}
		return true
	}
	walk(root, 0)
	sort.Slice(files, func(i, j int) bool {
		if !files[i].modTime.Equal(files[j].modTime) {
			return files[i].modTime.After(files[j].modTime)
		}
		// Rollout names start with a timestamp; prefer the newer one when
		// several files share a modification time (for example after a copy).
		return files[i].path > files[j].path
	})
	if len(files) > sessionMaxFiles {
		files = files[:sessionMaxFiles]
	}
	paths := make([]string, len(files))
	for i, file := range files {
		paths[i] = file.path
	}
	return paths
}

// numericDir matches the YYYY (depth 0) and MM or DD (deeper) directory names
// used by Codex's session tree.
func numericDir(name string, depth int) bool {
	width := 2
	if depth == 0 {
		width = 4
	}
	if len(name) != width {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' {
			return false
		}
	}
	return true
}

func isRolloutFile(name string) bool {
	return strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl")
}

// lastRateLimitsInFile scans the tail of one rollout file backwards for the
// newest token_count event that carries rate limits.
func lastRateLimitsInFile(path string, now time.Time) (sessionObservation, bool) {
	file, err := os.Open(path)
	if err != nil {
		return sessionObservation{}, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return sessionObservation{}, false
	}
	offset := int64(0)
	if info.Size() > sessionTailBytes {
		offset = info.Size() - sessionTailBytes
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return sessionObservation{}, false
	}
	tail, err := io.ReadAll(io.LimitReader(file, sessionTailBytes))
	if err != nil {
		return sessionObservation{}, false
	}
	if offset > 0 {
		// Drop the partial first line.
		if cut := bytes.IndexByte(tail, '\n'); cut >= 0 {
			tail = tail[cut+1:]
		} else {
			return sessionObservation{}, false
		}
	}
	lines := bytes.Split(tail, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if obs, ok := parseRolloutLine(lines[i], now); ok {
			return obs, true
		}
	}
	return sessionObservation{}, false
}

type sessionWindow struct {
	UsedPercent     *float64 `json:"used_percent"`
	WindowMinutes   *int64   `json:"window_minutes"`
	ResetsAt        *int64   `json:"resets_at"`
	ResetsInSeconds *int64   `json:"resets_in_seconds"`
}

func parseRolloutLine(line []byte, now time.Time) (sessionObservation, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || !bytes.Contains(line, []byte(`"token_count"`)) {
		return sessionObservation{}, false
	}
	var doc struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   struct {
			Type       string `json:"type"`
			RateLimits *struct {
				LimitID   *string        `json:"limit_id"`
				Primary   *sessionWindow `json:"primary"`
				Secondary *sessionWindow `json:"secondary"`
				PlanType  *string        `json:"plan_type"`
			} `json:"rate_limits"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &doc) != nil || doc.Type != "event_msg" || doc.Payload.Type != "token_count" || doc.Payload.RateLimits == nil {
		return sessionObservation{}, false
	}
	limits := doc.Payload.RateLimits
	// Per-model limits use their own limit_id; only the main Codex limit is
	// the account's subscription window.
	if limits.LimitID != nil && *limits.LimitID != "" && *limits.LimitID != "codex" {
		return sessionObservation{}, false
	}
	observed, err := time.Parse(time.RFC3339Nano, doc.Timestamp)
	if err != nil || observed.After(now.Add(5*time.Minute)) {
		return sessionObservation{}, false
	}
	var windows []Window
	for _, w := range []*sessionWindow{limits.Primary, limits.Secondary} {
		if w == nil || w.UsedPercent == nil {
			continue
		}
		out := Window{UsedPercent: percentOf(*w.UsedPercent), Kind: KindOther}
		if w.WindowMinutes != nil && *w.WindowMinutes > 0 {
			out.WindowSeconds = *w.WindowMinutes * 60
			out.Kind = kindForSeconds(out.WindowSeconds)
		}
		switch {
		case w.ResetsAt != nil && *w.ResetsAt > 0:
			out.ResetsAt = time.Unix(*w.ResetsAt, 0).UTC().Format(time.RFC3339)
		case w.ResetsInSeconds != nil && *w.ResetsInSeconds >= 0:
			out.ResetsAt = observed.Add(time.Duration(*w.ResetsInSeconds) * time.Second).UTC().Format(time.RFC3339)
		}
		windows = append(windows, out)
	}
	if len(windows) == 0 {
		return sessionObservation{}, false
	}
	sortWindows(windows)
	plan := ""
	if limits.PlanType != nil {
		plan = codexPlan(*limits.PlanType)
	}
	return sessionObservation{observedAt: observed.UTC(), plan: plan, windows: windows}, true
}

// expireResetWindows marks windows whose reset time passed after the
// observation: the logged percentage no longer applies, and usage since the
// reset is unknown until a live query or a newer session. UsedPercent is
// cleared so the report cannot claim a known balance.
func expireResetWindows(windows []Window, now time.Time) []Window {
	out := make([]Window, len(windows))
	for i, w := range windows {
		out[i] = w
		if w.ResetsAt == "" {
			continue
		}
		if reset, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil && !reset.After(now) {
			out[i].UsedPercent = nil
			out[i].ResetsAt = ""
			out[i].ResetSinceObserved = true
		}
	}
	return out
}
