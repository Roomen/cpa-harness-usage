// Package quota reports subscription rate-limit windows for Claude and Codex
// accounts. It is independent of internal/usage: provider quota percentages and
// local token counters are different metrics and are never combined.
package quota

// SchemaVersion is the version of the quota report JSON.
const SchemaVersion = 1

const (
	ProviderClaude = "claude"
	ProviderCodex  = "codex"

	OriginLocal = "local"
	OriginPool  = "pool"

	StatusOK    = "ok"
	StatusError = "error"

	// SourceLive marks data fetched from the provider during this request.
	SourceLive = "live"
	// SourceCache marks provider data served from the in-memory cache.
	SourceCache = "cache"
	// SourceSessionLog marks data read from the newest local Codex session log.
	SourceSessionLog = "session_log"

	KindSession = "session"
	KindDaily   = "daily"
	KindWeekly  = "weekly"
	KindMonthly = "monthly"
	KindOther   = "other"
)

// Safe error codes. They never carry upstream messages, paths, or identities.
const (
	ErrNotConfigured  = "not_configured"
	ErrTokenExpired   = "token_expired"
	ErrUpstream       = "upstream_error"
	ErrRateLimited    = "rate_limited"
	ErrKeychainDenied = "keychain_denied"
	ErrUnknownSchema  = "unknown_schema"
	ErrCanceled       = "canceled"
)

// Pool status values describe the CPA credential pool as a whole.
const (
	PoolOK          = "ok"
	PoolUnavailable = "unavailable"
	PoolError       = "error"
)

type Report struct {
	SchemaVersion int    `json:"schemaVersion"`
	GeneratedAt   string `json:"generatedAt"`
	// RefreshedAt is when provider data was last queried; empty before the
	// first refresh.
	RefreshedAt string    `json:"refreshedAt,omitempty"`
	PoolStatus  string    `json:"poolStatus"`
	Accounts    []Account `json:"accounts"`
}

// Account is one deduplicated provider account. It carries display labels
// only: no token, email, account ID, file name, or path.
type Account struct {
	// ID is an opaque, stable key for the page: local-claude, local-codex, or
	// pool-<n>.
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Origin   string `json:"origin"`
	// PoolNumber is the credential's position among CPA claude/codex
	// credentials sorted by auth index (1-based).
	PoolNumber int `json:"poolNumber,omitempty"`
	// MergedPool lists pool numbers that are the same account as this card
	// and were therefore not queried or shown separately.
	MergedPool []int  `json:"mergedPool,omitempty"`
	Plan       string `json:"plan,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	// LiveError keeps the live-query failure when a session-log fallback
	// supplied the windows instead.
	LiveError  string   `json:"liveError,omitempty"`
	Source     string   `json:"source,omitempty"`
	ObservedAt string   `json:"observedAt,omitempty"`
	Windows    []Window `json:"windows"`
}

type Window struct {
	Kind          string `json:"kind"`
	WindowSeconds int64  `json:"windowSeconds,omitempty"`
	// UsedPercent is the used percentage (0-100). It is absent when the usage
	// is unknown, for example a session-log window whose reset time has
	// passed; a missing value must not be read as 0% used.
	UsedPercent *float64 `json:"usedPercent,omitempty"`
	ResetsAt    string   `json:"resetsAt,omitempty"`
	// ResetSinceObserved marks a session-log window whose reset time has
	// passed since it was logged. UsedPercent is absent in that case.
	ResetSinceObserved bool `json:"resetSinceObserved,omitempty"`
}

// kindForSeconds classifies a window by its length, never by the upstream
// field name, because providers reorder primary/secondary windows.
func kindForSeconds(seconds int64) string {
	switch seconds {
	case 5 * 3600:
		return KindSession
	case 24 * 3600:
		return KindDaily
	case 7 * 24 * 3600:
		return KindWeekly
	case 30 * 24 * 3600:
		return KindMonthly
	default:
		return KindOther
	}
}

func clampPercent(value float64) float64 {
	if value != value || value < 0 { // NaN or negative
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

// percentOf returns a reportable used percentage, or nil when unknown.
func percentOf(value float64) *float64 {
	out := clampPercent(value)
	return &out
}
