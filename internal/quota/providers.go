package quota

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	claudeUsageURL   = "https://api.anthropic.com/api/oauth/usage"
	claudeProfileURL = "https://api.anthropic.com/api/oauth/profile"
	claudeBeta       = "oauth-2025-04-20"
	codexUsageURL    = "https://chatgpt.com/backend-api/wham/usage"
)

// fetchError carries a safe error code from a provider request.
type fetchError struct{ code string }

func (e *fetchError) Error() string { return e.code }

func codeOf(err error) string {
	var fe *fetchError
	if errors.As(err, &fe) {
		return fe.code
	}
	switch {
	case errors.Is(err, errNotConfigured):
		return ErrNotConfigured
	case errors.Is(err, errKeychainDenied):
		return ErrKeychainDenied
	case errors.Is(err, context.Canceled):
		return ErrCanceled
	}
	return ErrUpstream
}

// statusError maps an upstream status to a safe code. 401/403 never trigger
// a token refresh here: refreshing rotates the refresh token, which would log
// the local CLI out, and CPA refreshes its own pool credentials.
func statusError(status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == 401 || status == 403:
		return &fetchError{ErrTokenExpired}
	case status == 429:
		return &fetchError{ErrRateLimited}
	default:
		return &fetchError{ErrUpstream}
	}
}

type providerClient struct {
	host      HostFunc
	userAgent string
}

func (c providerClient) get(ctx context.Context, url string, headers map[string][]string) ([]byte, error) {
	headers["User-Agent"] = []string{c.userAgent}
	headers["Accept"] = []string{"application/json"}
	res, err := c.host.do(ctx, httpRequest{Method: "GET", URL: url, Headers: headers})
	if err != nil {
		if ctx.Err() != nil {
			return nil, context.Canceled
		}
		return nil, &fetchError{ErrUpstream}
	}
	if err := statusError(res.StatusCode); err != nil {
		return nil, err
	}
	return res.Body, nil
}

func claudeHeaders(token string) map[string][]string {
	return map[string][]string{
		"Authorization":  {"Bearer " + token},
		"anthropic-beta": {claudeBeta},
	}
}

func (c providerClient) claudeUsage(ctx context.Context, token string) ([]Window, error) {
	body, err := c.get(ctx, claudeUsageURL, claudeHeaders(token))
	if err != nil {
		return nil, err
	}
	return parseClaudeUsage(body)
}

// claudeProfile is the subset of /api/oauth/profile used for deduplication
// and the plan label.
type claudeProfile struct {
	accountUUID string
	plan        string
}

func (c providerClient) claudeProfile(ctx context.Context, token string) (claudeProfile, error) {
	body, err := c.get(ctx, claudeProfileURL, claudeHeaders(token))
	if err != nil {
		return claudeProfile{}, err
	}
	return parseClaudeProfile(body)
}

func (c providerClient) codexUsage(ctx context.Context, token, accountID string) ([]Window, string, error) {
	headers := map[string][]string{"Authorization": {"Bearer " + token}}
	if accountID != "" {
		headers["chatgpt-account-id"] = []string{accountID}
	}
	body, err := c.get(ctx, codexUsageURL, headers)
	if err != nil {
		return nil, "", err
	}
	return parseCodexUsage(body)
}

// parseClaudeUsage reads five_hour and seven_day. utilization is the used
// percentage (0-100, possibly fractional); resets_at is ISO 8601 and may be
// null for an unused window. Other members (limits, extra_usage, spend) are
// ignored.
func parseClaudeUsage(body []byte) ([]Window, error) {
	type window struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    *string  `json:"resets_at"`
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil || doc == nil {
		return nil, &fetchError{ErrUnknownSchema}
	}
	windows := make([]Window, 0, 2)
	for _, field := range []struct {
		name    string
		seconds int64
	}{{"five_hour", 5 * 3600}, {"seven_day", 7 * 24 * 3600}} {
		raw, ok := doc[field.name]
		if !ok || strings.TrimSpace(string(raw)) == "null" {
			continue
		}
		var w window
		if json.Unmarshal(raw, &w) != nil || w.Utilization == nil {
			return nil, &fetchError{ErrUnknownSchema}
		}
		out := Window{Kind: kindForSeconds(field.seconds), WindowSeconds: field.seconds, UsedPercent: percentOf(*w.Utilization)}
		if w.ResetsAt != nil {
			if reset, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*w.ResetsAt)); err == nil {
				out.ResetsAt = reset.UTC().Format(time.RFC3339)
			}
		}
		windows = append(windows, out)
	}
	if len(windows) == 0 {
		return nil, &fetchError{ErrUnknownSchema}
	}
	return windows, nil
}

func parseClaudeProfile(body []byte) (claudeProfile, error) {
	var doc struct {
		Account struct {
			UUID         string `json:"uuid"`
			HasClaudeMax bool   `json:"has_claude_max"`
			HasClaudePro bool   `json:"has_claude_pro"`
		} `json:"account"`
		Organization struct {
			OrganizationType string `json:"organization_type"`
			RateLimitTier    string `json:"rate_limit_tier"`
		} `json:"organization"`
	}
	if json.Unmarshal(body, &doc) != nil || strings.TrimSpace(doc.Account.UUID) == "" {
		return claudeProfile{}, &fetchError{ErrUnknownSchema}
	}
	subscription := doc.Organization.OrganizationType
	if subscription == "" {
		switch {
		case doc.Account.HasClaudeMax:
			subscription = "max"
		case doc.Account.HasClaudePro:
			subscription = "pro"
		}
	}
	return claudeProfile{
		accountUUID: strings.TrimSpace(doc.Account.UUID),
		plan:        claudePlan(subscription, doc.Organization.RateLimitTier),
	}, nil
}

// parseCodexUsage reads rate_limit.primary_window/secondary_window. Windows
// are classified by limit_window_seconds, not by primary/secondary.
// additional_rate_limits are not reported. A response that does not contain
// the documented rate_limit object is unknown_schema, not an empty success:
// otherwise a renamed field would be cached as "no quota windows" and hide
// the session-log fallback.
func parseCodexUsage(body []byte) ([]Window, string, error) {
	type window struct {
		UsedPercent        *float64 `json:"used_percent"`
		ResetAt            *int64   `json:"reset_at"`
		LimitWindowSeconds int64    `json:"limit_window_seconds"`
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil || doc == nil {
		return nil, "", &fetchError{ErrUnknownSchema}
	}
	rawLimit, ok := doc["rate_limit"]
	if !ok {
		return nil, "", &fetchError{ErrUnknownSchema}
	}
	plan := ""
	if rawPlan, ok := doc["plan_type"]; ok && strings.TrimSpace(string(rawPlan)) != "null" {
		if json.Unmarshal(rawPlan, &plan) != nil {
			return nil, "", &fetchError{ErrUnknownSchema}
		}
	}
	windows := []Window{}
	if strings.TrimSpace(string(rawLimit)) != "null" {
		var limits map[string]json.RawMessage
		if json.Unmarshal(rawLimit, &limits) != nil || limits == nil {
			return nil, "", &fetchError{ErrUnknownSchema}
		}
		rawPrimary, hasPrimary := limits["primary_window"]
		rawSecondary, hasSecondary := limits["secondary_window"]
		if !hasPrimary && !hasSecondary {
			return nil, "", &fetchError{ErrUnknownSchema}
		}
		for _, raw := range []json.RawMessage{rawPrimary, rawSecondary} {
			if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
				continue
			}
			var w window
			if json.Unmarshal(raw, &w) != nil || w.UsedPercent == nil || w.LimitWindowSeconds <= 0 {
				return nil, "", &fetchError{ErrUnknownSchema}
			}
			out := Window{Kind: kindForSeconds(w.LimitWindowSeconds), WindowSeconds: w.LimitWindowSeconds, UsedPercent: percentOf(*w.UsedPercent)}
			if w.ResetAt != nil && *w.ResetAt > 0 {
				out.ResetsAt = time.Unix(*w.ResetAt, 0).UTC().Format(time.RFC3339)
			}
			windows = append(windows, out)
		}
	}
	sortWindows(windows)
	return windows, codexPlan(plan), nil
}

// sortWindows orders windows shortest first so cards read session → weekly.
func sortWindows(windows []Window) {
	for i := 1; i < len(windows); i++ {
		for j := i; j > 0 && windows[j].WindowSeconds < windows[j-1].WindowSeconds; j-- {
			windows[j], windows[j-1] = windows[j-1], windows[j]
		}
	}
}
