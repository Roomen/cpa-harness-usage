package quota

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// credential is an access token plus the labels needed for display and
// deduplication. It never leaves this package.
type credential struct {
	token     string
	accountID string // Codex ChatGPT account ID; Claude account UUID when known
	plan      string
	expiresAt time.Time
}

var (
	errNotConfigured  = errors.New(ErrNotConfigured)
	errKeychainDenied = errors.New(ErrKeychainDenied)
)

// LocalCredentials reads the accounts that Claude Code and Codex CLI are
// logged in with on this machine. Implementations must be read-only: tokens
// are never refreshed or written back.
type LocalCredentials interface {
	Claude() (credential, error)
	Codex() (credential, error)
}

// keychainReader reads a macOS generic password by service. It is nil where
// the Security framework is unavailable.
type keychainReader func(service string) ([]byte, error)

const claudeKeychainService = "Claude Code-credentials"

// fileCredentials is the production reader. Environment lookups and the
// keychain are injectable so tests never touch real credentials.
type fileCredentials struct {
	getenv   func(string) string
	home     func() (string, error)
	readFile func(string) ([]byte, error)
	keychain keychainReader
}

func newFileCredentials() *fileCredentials {
	return &fileCredentials{getenv: os.Getenv, home: os.UserHomeDir, readFile: readSmallFile, keychain: systemKeychain}
}

const maxCredentialFileSize = 256 << 10

func readSmallFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxCredentialFileSize {
		return nil, errNotConfigured
	}
	return io.ReadAll(io.LimitReader(file, maxCredentialFileSize))
}

func (f *fileCredentials) Claude() (credential, error) {
	if token := strings.TrimSpace(f.getenv("CLAUDE_CODE_OAUTH_TOKEN")); token != "" {
		return credential{token: token}, nil
	}
	if f.keychain != nil && !f.keychainDisabled() {
		raw, err := f.keychain(claudeKeychainService)
		switch {
		case err == nil:
			return parseClaudeCredentials(raw)
		case errors.Is(err, errKeychainDenied):
			return credential{}, err
		}
		// Not found in the keychain: Claude Code falls back to the file.
	}
	dir := strings.TrimSpace(f.getenv("CLAUDE_CONFIG_DIR"))
	if dir == "" {
		home, err := f.home()
		if err != nil || home == "" {
			return credential{}, errNotConfigured
		}
		dir = filepath.Join(home, ".claude")
	}
	raw, err := f.readFile(filepath.Join(dir, ".credentials.json"))
	if err != nil {
		return credential{}, errNotConfigured
	}
	return parseClaudeCredentials(raw)
}
func (f *fileCredentials) keychainDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(f.getenv("CLAUDE_DISABLE_KEYCHAIN")))
	if v == "" {
		v = strings.ToLower(strings.TrimSpace(f.getenv("CPA_DISABLE_KEYCHAIN")))
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}


func (f *fileCredentials) Codex() (credential, error) {
	raw, err := f.readFile(filepath.Join(f.codexHome(), "auth.json"))
	if err != nil {
		return credential{}, errNotConfigured
	}
	return parseCodexAuth(raw)
}

func (f *fileCredentials) codexHome() string {
	if dir := strings.TrimSpace(f.getenv("CODEX_HOME")); dir != "" {
		return dir
	}
	home, err := f.home()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// parseClaudeCredentials accepts {"claudeAiOauth":{...}} or the OAuth fields
// at the root.
func parseClaudeCredentials(raw []byte) (credential, error) {
	type oauth struct {
		AccessToken      string          `json:"accessToken"`
		ExpiresAt        json.RawMessage `json:"expiresAt"`
		SubscriptionType string          `json:"subscriptionType"`
		RateLimitTier    string          `json:"rateLimitTier"`
	}
	var doc struct {
		ClaudeAiOauth *oauth `json:"claudeAiOauth"`
		oauth
	}
	if json.Unmarshal(raw, &doc) != nil {
		return credential{}, errNotConfigured
	}
	o := doc.oauth
	if doc.ClaudeAiOauth != nil {
		o = *doc.ClaudeAiOauth
	}
	if strings.TrimSpace(o.AccessToken) == "" {
		return credential{}, errNotConfigured
	}
	return credential{
		token:     strings.TrimSpace(o.AccessToken),
		plan:      claudePlan(o.SubscriptionType, o.RateLimitTier),
		expiresAt: parseExpiry(o.ExpiresAt),
	}, nil
}

// parseExpiry reads epoch milliseconds (Claude Code) or an RFC 3339 string.
func parseExpiry(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	var millis float64
	if json.Unmarshal(raw, &millis) == nil && millis > 0 {
		return time.UnixMilli(int64(millis))
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(text)); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// parseCodexAuth reads Codex CLI's auth.json. API-key-only logins have no
// ChatGPT rate-limit windows and count as not configured.
func parseCodexAuth(raw []byte) (credential, error) {
	var doc struct {
		Tokens *struct {
			IDToken     json.RawMessage `json:"id_token"`
			AccessToken string          `json:"access_token"`
			AccountID   string          `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Tokens == nil || strings.TrimSpace(doc.Tokens.AccessToken) == "" {
		return credential{}, errNotConfigured
	}
	claims := codexIDClaims(doc.Tokens.IDToken)
	accountID := strings.TrimSpace(doc.Tokens.AccountID)
	if accountID == "" {
		accountID = claims.ChatGPTAccountID
	}
	return credential{
		token:     strings.TrimSpace(doc.Tokens.AccessToken),
		accountID: accountID,
		plan:      codexPlan(claims.ChatGPTPlanType),
	}, nil
}

// parseCodexPool reads a CPA codex credential file.
func parseCodexPool(raw []byte) (credential, error) {
	var doc struct {
		IDToken     json.RawMessage `json:"id_token"`
		AccessToken string          `json:"access_token"`
		AccountID   string          `json:"account_id"`
		PlanType    string          `json:"plan_type"`
		Expired     string          `json:"expired"`
	}
	if json.Unmarshal(raw, &doc) != nil || strings.TrimSpace(doc.AccessToken) == "" {
		return credential{}, errNotConfigured
	}
	claims := codexIDClaims(doc.IDToken)
	accountID := strings.TrimSpace(doc.AccountID)
	if accountID == "" {
		accountID = claims.ChatGPTAccountID
	}
	plan := doc.PlanType
	if plan == "" {
		plan = claims.ChatGPTPlanType
	}
	return credential{
		token:     strings.TrimSpace(doc.AccessToken),
		accountID: accountID,
		plan:      codexPlan(plan),
		expiresAt: parseRFC3339(doc.Expired),
	}, nil
}

// parseClaudePool reads a CPA claude credential file.
func parseClaudePool(raw []byte) (credential, error) {
	var doc struct {
		AccessToken string `json:"access_token"`
		AccountUUID string `json:"account_uuid"`
		Expired     string `json:"expired"`
	}
	if json.Unmarshal(raw, &doc) != nil || strings.TrimSpace(doc.AccessToken) == "" {
		return credential{}, errNotConfigured
	}
	return credential{
		token:     strings.TrimSpace(doc.AccessToken),
		accountID: strings.TrimSpace(doc.AccountUUID),
		expiresAt: parseRFC3339(doc.Expired),
	}, nil
}

func parseRFC3339(text string) time.Time {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(text))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

type codexClaims struct {
	ChatGPTAccountID string `json:"chatgpt_account_id"`
	ChatGPTPlanType  string `json:"chatgpt_plan_type"`
}

// codexIDClaims decodes the unverified id_token payload. It is used only for
// labels and deduplication, never for authorization. id_token is a JWT string
// or, in older CPA files, an object holding the raw JWT.
func codexIDClaims(raw json.RawMessage) codexClaims {
	var jwt string
	if json.Unmarshal(raw, &jwt) != nil {
		var wrapped struct {
			RawJWT string `json:"raw_jwt"`
		}
		if json.Unmarshal(raw, &wrapped) != nil {
			return codexClaims{}
		}
		jwt = wrapped.RawJWT
	}
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return codexClaims{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return codexClaims{}
	}
	var body struct {
		Auth codexClaims `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &body) != nil {
		return codexClaims{}
	}
	return body.Auth
}

// claudePlan maps Claude Code's subscription labels to display names.
func claudePlan(subscription, tier string) string {
	tier = strings.ToLower(tier)
	switch {
	case strings.Contains(tier, "max_20x"):
		return "Max 20x"
	case strings.Contains(tier, "max_5x"):
		return "Max 5x"
	}
	switch strings.ToLower(strings.TrimSpace(subscription)) {
	case "max", "claude_max":
		return "Max"
	case "pro", "claude_pro":
		return "Pro"
	case "team", "claude_team":
		return "Team"
	case "enterprise", "claude_enterprise":
		return "Enterprise"
	}
	return ""
}

// codexPlan title-cases ChatGPT plan types and drops anything that is not a
// short plain label.
func codexPlan(plan string) string {
	plan = strings.ToLower(strings.TrimSpace(plan))
	if plan == "" || len(plan) > 24 {
		return ""
	}
	for _, r := range plan {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return ""
		}
	}
	words := strings.FieldsFunc(plan, func(r rune) bool { return r == '_' || r == '-' })
	for i, word := range words {
		words[i] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, " ")
}
