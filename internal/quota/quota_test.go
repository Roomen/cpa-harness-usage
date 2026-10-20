package quota

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Synthetic secrets only; none of these are real credentials.
const (
	localClaudeToken = "synthetic-local-claude-token"
	localCodexToken  = "synthetic-local-codex-token"
	poolClaudeToken  = "synthetic-pool-claude-token"
	poolCodexToken   = "synthetic-pool-codex-token"
	codexAccount     = "acct-synthetic-0001"
	claudeUUID       = "uuid-synthetic-claude-0001"
	syntheticEmail   = "someone@example.invalid"
)

var fixedNow = time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)

type fakeLocal struct {
	claude, codex       credential
	claudeErr, codexErr error
}

func (f fakeLocal) Claude() (credential, error) { return f.claude, f.claudeErr }
func (f fakeLocal) Codex() (credential, error)  { return f.codex, f.codexErr }

type fakeHTTP struct {
	status int
	body   string
}

// fakeHost implements the CPA host callbacks used by the service.
type fakeHost struct {
	mu       sync.Mutex
	handler  func(method, url string, headers map[string][]string) fakeHTTP
	files    []map[string]any
	authJSON map[string]string
	calls    map[string]int
	canceled []string
	block    chan struct{}
	listErr  bool
}

func newFakeHost(handler func(method, url string, headers map[string][]string) fakeHTTP) *fakeHost {
	return &fakeHost{handler: handler, authJSON: map[string]string{}, calls: map[string]int{}}
}

func (f *fakeHost) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

func ok(result any) []byte {
	raw, _ := json.Marshal(map[string]any{"ok": true, "result": result})
	return raw
}

func (f *fakeHost) fn(method string, request []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls[method]++
	block := f.block
	f.mu.Unlock()
	switch method {
	case methodHTTPOpen:
		return ok(map[string]string{"operation_id": "7"}), nil
	case methodHTTPCancel:
		var req map[string]string
		_ = json.Unmarshal(request, &req)
		f.mu.Lock()
		f.canceled = append(f.canceled, req["operation_id"])
		f.mu.Unlock()
		return ok(struct{}{}), nil
	case methodHTTPDo:
		var req struct {
			OperationID string              `json:"operation_id"`
			Method      string              `json:"method"`
			URL         string              `json:"url"`
			Headers     map[string][]string `json:"headers"`
		}
		if err := json.Unmarshal(request, &req); err != nil || req.OperationID != "7" {
			return []byte(`{"ok":false,"error":{"code":"bad","message":"bad"}}`), nil
		}
		f.mu.Lock()
		f.calls[req.URL]++
		f.mu.Unlock()
		if block != nil {
			<-block
		}
		res := f.handler(req.Method, req.URL, req.Headers)
		return ok(map[string]any{"StatusCode": res.status, "Headers": map[string][]string{}, "Body": []byte(res.body)}), nil
	case methodAuthList:
		if f.listErr {
			return []byte(`{"ok":false,"error":{"code":"host_call_failed","message":"auth directory is unavailable"}}`), nil
		}
		return ok(map[string]any{"files": f.files}), nil
	case methodAuthGet:
		var req map[string]string
		_ = json.Unmarshal(request, &req)
		raw, found := f.authJSON[req["auth_index"]]
		if !found {
			return []byte(`{"ok":false,"error":{"code":"host_call_failed","message":"not found"}}`), nil
		}
		return ok(map[string]any{"auth_index": req["auth_index"], "name": "secret-file-name.json", "path": "/secret/path/auth.json", "json": json.RawMessage(raw)}), nil
	}
	return nil, errors.New("unexpected method")
}

func jwt(claims map[string]any) string {
	payload, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": claims, "email": syntheticEmail})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

const (
	claudeUsageOK  = `{"five_hour":{"utilization":63.5,"resets_at":"2026-10-20T09:52:00.123456+00:00"},"seven_day":{"utilization":41,"resets_at":"2026-10-23T13:00:00Z"},"seven_day_opus":null,"extra_usage":{"is_enabled":false}}`
	codexUsageOK   = `{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":88,"limit_window_seconds":18000,"reset_at":1792400000},"secondary_window":{"used_percent":72.5,"limit_window_seconds":604800,"reset_at":1792900000}}}`
	claudeProfileX = `{"account":{"uuid":"` + claudeUUID + `","email_address":"` + syntheticEmail + `","has_claude_max":true},"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_5x"}}`
)

func standardHandler(method, url string, headers map[string][]string) fakeHTTP {
	switch url {
	case claudeUsageURL:
		return fakeHTTP{200, claudeUsageOK}
	case claudeProfileURL:
		return fakeHTTP{200, claudeProfileX}
	case codexUsageURL:
		return fakeHTTP{200, codexUsageOK}
	}
	return fakeHTTP{404, `{}`}
}

func noSessions(time.Time) (sessionObservation, bool) { return sessionObservation{}, false }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestService(local LocalCredentials, host *fakeHost, sessions sessionReader) (*Service, *clock) {
	c := &clock{t: fixedNow}
	if sessions == nil {
		sessions = noSessions
	}
	s := New(Config{Local: local, Sessions: sessions, UserAgent: "cpa-harness-usage/test", Now: c.now})
	if host != nil {
		s.SetHost(host.fn)
	}
	return s, c
}

func decode(t *testing.T, status int, body []byte) Report {
	t.Helper()
	if status != 200 {
		t.Fatalf("status = %d body=%s", status, body)
	}
	var report Report
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func snap(t *testing.T, s *Service, refresh bool) Report {
	t.Helper()
	status, body := s.Snapshot(refresh)
	return decode(t, status, body)
}

func find(report Report, id string) *Account {
	for i := range report.Accounts {
		if report.Accounts[i].ID == id {
			return &report.Accounts[i]
		}
	}
	return nil
}

func TestParseClaudeUsage(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
		want    []Window
	}{
		{"fractional utilization and extra fields", claudeUsageOK, false, []Window{
			{Kind: KindSession, WindowSeconds: 18000, UsedPercent: percentOf(63.5), ResetsAt: "2026-10-20T09:52:00Z"},
			{Kind: KindWeekly, WindowSeconds: 604800, UsedPercent: percentOf(41), ResetsAt: "2026-10-23T13:00:00Z"},
		}},
		{"null window and null reset", `{"five_hour":null,"seven_day":{"utilization":0,"resets_at":null}}`, false, []Window{
			{Kind: KindWeekly, WindowSeconds: 604800, UsedPercent: percentOf(0)},
		}},
		{"out of range is clamped", `{"five_hour":{"utilization":130,"resets_at":"2026-10-20T09:00:00Z"}}`, false, []Window{
			{Kind: KindSession, WindowSeconds: 18000, UsedPercent: percentOf(100), ResetsAt: "2026-10-20T09:00:00Z"},
		}},
		{"missing utilization", `{"five_hour":{"resets_at":"2026-10-20T09:00:00Z"}}`, true, nil},
		{"no windows", `{"limits":[]}`, true, nil},
		{"not json", `<html>`, true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseClaudeUsage([]byte(tc.body))
			if tc.wantErr {
				if codeOf(err) != ErrUnknownSchema {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("windows = %+v", got)
			}
			for i := range got {
				if !sameWindow(got[i], tc.want[i]) {
					t.Fatalf("window %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// sameWindow compares windows by value; UsedPercent is a pointer.
func sameWindow(a, b Window) bool {
	if a.Kind != b.Kind || a.WindowSeconds != b.WindowSeconds || a.ResetsAt != b.ResetsAt || a.ResetSinceObserved != b.ResetSinceObserved {
		return false
	}
	if (a.UsedPercent == nil) != (b.UsedPercent == nil) {
		return false
	}
	return a.UsedPercent == nil || *a.UsedPercent == *b.UsedPercent
}

func TestParseCodexUsage(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantErr   bool
		wantKinds []string
		wantPlan  string
	}{
		{"standard", codexUsageOK, false, []string{KindSession, KindWeekly}, "Plus"},
		{"windows classified by length, not name", `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":5,"limit_window_seconds":604800,"reset_at":1792900000},"secondary_window":{"used_percent":9,"limit_window_seconds":18000,"reset_at":1792400000}}}`, false, []string{KindSession, KindWeekly}, "Pro"},
		{"unknown and daily windows", `{"rate_limit":{"primary_window":{"used_percent":1.25,"limit_window_seconds":3600},"secondary_window":{"used_percent":2,"limit_window_seconds":86400}}}`, false, []string{KindOther, KindDaily}, ""},
		{"monthly", `{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":2592000}}}`, false, []string{KindMonthly}, ""},
		{"no rate limit", `{"plan_type":"free","rate_limit":null}`, false, []string{}, "Free"},
		{"null plan type", `{"plan_type":null,"rate_limit":null}`, false, []string{}, ""},
		{"missing used percent", `{"rate_limit":{"primary_window":{"limit_window_seconds":18000}}}`, true, nil, ""},
		{"top-level null", `null`, true, nil, ""},
		{"empty object", `{}`, true, nil, ""},
		{"unrelated object", `{"error":"synthetic"}`, true, nil, ""},
		{"rate limit without windows", `{"rate_limit":{}}`, true, nil, ""},
		{"rate limit not an object", `{"rate_limit":[]}`, true, nil, ""},
		{"not json", `nope`, true, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, plan, err := parseCodexUsage([]byte(tc.body))
			if tc.wantErr {
				if codeOf(err) != ErrUnknownSchema {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if plan != tc.wantPlan || len(got) != len(tc.wantKinds) {
				t.Fatalf("plan=%q windows=%+v", plan, got)
			}
			for i, kind := range tc.wantKinds {
				if got[i].Kind != kind {
					t.Fatalf("window %d kind = %s, want %s", i, got[i].Kind, kind)
				}
			}
		})
	}
}

func TestParseCredentials(t *testing.T) {
	t.Run("claude nested", func(t *testing.T) {
		cred, err := parseClaudeCredentials([]byte(`{"claudeAiOauth":{"accessToken":"tok","refreshToken":"r","expiresAt":1792400000000,"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"}}`))
		if err != nil || cred.token != "tok" || cred.plan != "Max 20x" || cred.expiresAt.Unix() != 1792400000 {
			t.Fatalf("cred=%+v err=%v", cred, err)
		}
	})
	t.Run("claude root", func(t *testing.T) {
		cred, err := parseClaudeCredentials([]byte(`{"accessToken":"tok","subscriptionType":"pro"}`))
		if err != nil || cred.token != "tok" || cred.plan != "Pro" {
			t.Fatalf("cred=%+v err=%v", cred, err)
		}
	})
	t.Run("claude missing token", func(t *testing.T) {
		if _, err := parseClaudeCredentials([]byte(`{"claudeAiOauth":{}}`)); !errors.Is(err, errNotConfigured) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("codex account from id token", func(t *testing.T) {
		raw := `{"OPENAI_API_KEY":null,"tokens":{"id_token":"` + jwt(map[string]any{"chatgpt_account_id": codexAccount, "chatgpt_plan_type": "pro"}) + `","access_token":"tok","refresh_token":"r"}}`
		cred, err := parseCodexAuth([]byte(raw))
		if err != nil || cred.accountID != codexAccount || cred.plan != "Pro" {
			t.Fatalf("cred=%+v err=%v", cred, err)
		}
	})
	t.Run("codex explicit account wins", func(t *testing.T) {
		raw := `{"tokens":{"id_token":"` + jwt(map[string]any{"chatgpt_account_id": "other"}) + `","access_token":"tok","account_id":"` + codexAccount + `"}}`
		if cred, err := parseCodexAuth([]byte(raw)); err != nil || cred.accountID != codexAccount {
			t.Fatalf("cred=%+v err=%v", cred, err)
		}
	})
	t.Run("codex api key only", func(t *testing.T) {
		if _, err := parseCodexAuth([]byte(`{"OPENAI_API_KEY":"sk-synthetic","tokens":null}`)); !errors.Is(err, errNotConfigured) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("codex pool", func(t *testing.T) {
		cred, err := parseCodexPool([]byte(`{"type":"codex","access_token":"tok","account_id":"` + codexAccount + `","plan_type":"team","expired":"2026-10-21T00:00:00Z"}`))
		if err != nil || cred.accountID != codexAccount || cred.plan != "Team" || cred.expiresAt.IsZero() {
			t.Fatalf("cred=%+v err=%v", cred, err)
		}
	})
}

func TestFileCredentialsSources(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"file-token"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"CLAUDE_CONFIG_DIR": dir}
	base := func(kc keychainReader) *fileCredentials {
		return &fileCredentials{
			getenv:   func(k string) string { return env[k] },
			home:     func() (string, error) { return "", errors.New("no home in tests") },
			readFile: readSmallFile,
			keychain: kc,
		}
	}
	notFound := func(string) ([]byte, error) { return nil, errNotConfigured }
	if cred, err := base(notFound).Claude(); err != nil || cred.token != "file-token" {
		t.Fatalf("file fallback cred=%+v err=%v", cred, err)
	}
	var asked string
	keychain := func(service string) ([]byte, error) {
		asked = service
		return []byte(`{"claudeAiOauth":{"accessToken":"keychain-token"}}`), nil
	}
	if cred, err := base(keychain).Claude(); err != nil || cred.token != "keychain-token" || asked != claudeKeychainService {
		t.Fatalf("keychain cred=%+v err=%v service=%q", cred, err, asked)
	}
	denied := func(string) ([]byte, error) { return nil, errKeychainDenied }
	if _, err := base(denied).Claude(); codeOf(err) != ErrKeychainDenied {
		t.Fatalf("denied err = %v", err)
	}
	env["CLAUDE_CODE_OAUTH_TOKEN"] = "env-token"
	if cred, err := base(denied).Claude(); err != nil || cred.token != "env-token" {
		t.Fatalf("env cred=%+v err=%v", cred, err)
	}
	delete(env, "CLAUDE_CODE_OAUTH_TOKEN")
	delete(env, "CLAUDE_CONFIG_DIR")
	if _, err := base(nil).Claude(); !errors.Is(err, errNotConfigured) {
		t.Fatalf("missing err = %v", err)
	}
	if _, err := base(nil).Codex(); !errors.Is(err, errNotConfigured) {
		t.Fatalf("codex missing err = %v", err)
	}
}

func TestStatusCodesNeverRefreshTokens(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{{401, ErrTokenExpired}, {403, ErrTokenExpired}, {429, ErrRateLimited}, {500, ErrUpstream}, {302, ErrUpstream}}
	for _, tc := range cases {
		host := newFakeHost(func(method, url string, headers map[string][]string) fakeHTTP {
			return fakeHTTP{tc.status, `{"error":"synthetic"}`}
		})
		s, _ := newTestService(fakeLocal{codex: credential{token: localCodexToken, accountID: codexAccount}, claudeErr: errNotConfigured}, host, nil)
		report := snap(t, s, true)
		a := find(report, "local-codex")
		if a == nil || a.Status != StatusError || a.Error != tc.want || len(a.Windows) != 0 {
			t.Fatalf("status %d: account = %+v", tc.status, a)
		}
		for key := range host.calls {
			if strings.Contains(key, "token") || strings.Contains(key, "oauth/token") {
				t.Fatalf("token endpoint was called: %s", key)
			}
		}
		if host.count(codexUsageURL) != 1 {
			t.Fatalf("status %d: requests = %d", tc.status, host.count(codexUsageURL))
		}
	}
}

func TestExpiredTokenIsNotSent(t *testing.T) {
	host := newFakeHost(standardHandler)
	s, _ := newTestService(fakeLocal{claude: credential{token: localClaudeToken, expiresAt: fixedNow.Add(-time.Minute)}, codexErr: errNotConfigured}, host, nil)
	report := snap(t, s, true)
	if a := find(report, "local-claude"); a == nil || a.Error != ErrTokenExpired {
		t.Fatalf("account = %+v", a)
	}
	if host.count(methodHTTPDo) != 0 {
		t.Fatalf("expired token was sent %d times", host.count(methodHTTPDo))
	}
}

func TestRequestsCarryExpectedHeaders(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]map[string][]string{}
	host := newFakeHost(func(method, url string, headers map[string][]string) fakeHTTP {
		mu.Lock()
		seen[url] = headers
		mu.Unlock()
		return standardHandler(method, url, headers)
	})
	s, _ := newTestService(fakeLocal{
		claude: credential{token: localClaudeToken},
		codex:  credential{token: localCodexToken, accountID: codexAccount},
	}, host, nil)
	_ = snap(t, s, true)
	usage := seen[claudeUsageURL]
	if got := usage["Authorization"]; len(got) != 1 || got[0] != "Bearer "+localClaudeToken {
		t.Fatalf("claude auth = %v", got)
	}
	if got := usage["anthropic-beta"]; len(got) != 1 || got[0] != claudeBeta {
		t.Fatalf("claude beta = %v", got)
	}
	if got := usage["User-Agent"]; len(got) != 1 || got[0] != "cpa-harness-usage/test" {
		t.Fatalf("user agent = %v", got)
	}
	codex := seen[codexUsageURL]
	if got := codex["chatgpt-account-id"]; len(got) != 1 || got[0] != codexAccount {
		t.Fatalf("codex account header = %v", got)
	}
	for url := range seen {
		if strings.Contains(url, "?") {
			t.Fatalf("unexpected query parameters: %s", url)
		}
	}
}

func poolHost() *fakeHost {
	host := newFakeHost(func(method, url string, headers map[string][]string) fakeHTTP {
		if url == claudeProfileURL {
			if headers["Authorization"][0] == "Bearer "+poolClaudeToken || headers["Authorization"][0] == "Bearer "+localClaudeToken {
				return fakeHTTP{200, claudeProfileX}
			}
			return fakeHTTP{200, `{"account":{"uuid":"uuid-other"},"organization":{"organization_type":"claude_pro"}}`}
		}
		return standardHandler(method, url, headers)
	})
	// Listed out of order: numbering follows auth index.
	host.files = []map[string]any{
		{"auth_index": "30", "provider": "codex", "email": syntheticEmail, "path": "/secret/c.json"},
		{"auth_index": "10", "provider": "claude", "email": syntheticEmail},
		{"auth_index": "20", "provider": "codex"},
		{"auth_index": "40", "provider": "claude"},
		{"auth_index": "50", "provider": "gemini"},
		{"auth_index": "60", "provider": "codex", "disabled": true},
		{"auth_index": "70", "provider": "claude", "runtime_only": true},
	}
	host.authJSON["10"] = `{"type":"claude","access_token":"` + poolClaudeToken + `","email":"` + syntheticEmail + `"}`
	host.authJSON["20"] = `{"type":"codex","access_token":"` + poolCodexToken + `","account_id":"` + codexAccount + `","email":"` + syntheticEmail + `"}`
	host.authJSON["30"] = `{"type":"codex","access_token":"synthetic-pool-codex-2","account_id":"` + codexAccount + `"}`
	host.authJSON["40"] = `{"type":"claude","access_token":"synthetic-pool-claude-2","account_uuid":"uuid-pool-only"}`
	return host
}

func TestDeduplicatesLocalAndPoolAccounts(t *testing.T) {
	host := poolHost()
	s, _ := newTestService(fakeLocal{
		claude: credential{token: localClaudeToken, plan: "Max 5x"},
		codex:  credential{token: localCodexToken, accountID: codexAccount},
	}, host, nil)
	status, body := s.Snapshot(true)
	report := decode(t, status, body)
	if report.PoolStatus != PoolOK {
		t.Fatalf("pool status = %s", report.PoolStatus)
	}
	ids := []string{}
	for _, a := range report.Accounts {
		ids = append(ids, a.ID)
	}
	// local claude absorbs pool #1 (same UUID via profile); local codex absorbs
	// pool #2 and #3 (same ChatGPT account); pool #4 is a distinct account.
	if strings.Join(ids, ",") != "local-claude,local-codex,pool-4" {
		t.Fatalf("accounts = %v", ids)
	}
	claude, codex := find(report, "local-claude"), find(report, "local-codex")
	if len(claude.MergedPool) != 1 || claude.MergedPool[0] != 1 || claude.Plan != "Max 5x" {
		t.Fatalf("claude = %+v", claude)
	}
	if len(codex.MergedPool) != 2 || codex.MergedPool[0] != 2 || codex.MergedPool[1] != 3 || codex.Plan != "Plus" {
		t.Fatalf("codex = %+v", codex)
	}
	if host.count(codexUsageURL) != 1 || host.count(claudeUsageURL) != 2 {
		t.Fatalf("usage queries codex=%d claude=%d", host.count(codexUsageURL), host.count(claudeUsageURL))
	}
	if claude.Source != SourceLive || claude.Status != StatusOK || len(claude.Windows) != 2 {
		t.Fatalf("claude = %+v", claude)
	}
	if p := find(report, "pool-4"); p == nil || p.Origin != OriginPool || p.PoolNumber != 4 {
		t.Fatalf("pool-4 = %+v", p)
	}

	for _, secret := range []string{localClaudeToken, localCodexToken, poolClaudeToken, poolCodexToken, "synthetic-pool", codexAccount, claudeUUID, "uuid-pool-only", syntheticEmail, "secret", ".json", "/"} {
		if secret == "/" {
			// Only RFC 3339 timestamps may contain no slash; ensure no path leaks.
			if strings.Contains(string(body), "/secret") || strings.Contains(string(body), "/Users") {
				t.Fatalf("path leaked: %s", body)
			}
			continue
		}
		if strings.Contains(string(body), secret) {
			t.Fatalf("report leaked %q: %s", secret, body)
		}
	}
}

func TestPoolDuplicatesWithoutLocalAccounts(t *testing.T) {
	host := poolHost()
	s, _ := newTestService(fakeLocal{claudeErr: errNotConfigured, codexErr: errNotConfigured}, host, nil)
	report := snap(t, s, true)
	codex := find(report, "pool-2")
	if codex == nil || len(codex.MergedPool) != 1 || codex.MergedPool[0] != 3 || find(report, "pool-3") != nil {
		t.Fatalf("pool duplicates not merged: %+v", report.Accounts)
	}
	if a := find(report, "local-claude"); a == nil || a.Error != ErrNotConfigured {
		t.Fatalf("local claude = %+v", a)
	}
}

func TestTokenRejectedFallsBackToSameAccountCredential(t *testing.T) {
	host := newFakeHost(func(method, url string, headers map[string][]string) fakeHTTP {
		if url == codexUsageURL && headers["Authorization"][0] == "Bearer "+localCodexToken {
			return fakeHTTP{401, `{}`}
		}
		return standardHandler(method, url, headers)
	})
	host.files = []map[string]any{{"auth_index": "1", "provider": "codex"}}
	host.authJSON["1"] = `{"access_token":"` + poolCodexToken + `","account_id":"` + codexAccount + `"}`
	s, _ := newTestService(fakeLocal{claudeErr: errNotConfigured, codex: credential{token: localCodexToken, accountID: codexAccount}}, host, nil)
	report := snap(t, s, true)
	a := find(report, "local-codex")
	if a == nil || a.Status != StatusOK || len(a.MergedPool) != 1 {
		t.Fatalf("account = %+v", a)
	}
}

func TestExpiredCredentialStillMergesWithSameAccount(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	host := newFakeHost(func(method, url string, headers map[string][]string) fakeHTTP {
		if url == codexUsageURL {
			mu.Lock()
			sent = append(sent, headers["Authorization"][0])
			mu.Unlock()
		}
		return standardHandler(method, url, headers)
	})
	host.files = []map[string]any{
		{"auth_index": "1", "provider": "codex"},
		{"auth_index": "2", "provider": "codex"},
	}
	host.authJSON["1"] = `{"access_token":"synthetic-expired-codex","account_id":"` + codexAccount + `","expired":"2026-10-20T07:00:00Z"}`
	host.authJSON["2"] = `{"access_token":"` + poolCodexToken + `","account_id":"` + codexAccount + `"}`

	// The local login is expired too; the usable pool credential must supply
	// the account's windows for the merged card.
	s, _ := newTestService(fakeLocal{
		claudeErr: errNotConfigured,
		codex:     credential{token: localCodexToken, accountID: codexAccount, expiresAt: fixedNow.Add(-time.Hour)},
	}, host, nil)
	report := snap(t, s, true)
	a := find(report, "local-codex")
	if a == nil || a.Status != StatusOK || len(a.MergedPool) != 2 || a.MergedPool[0] != 1 || a.MergedPool[1] != 2 {
		t.Fatalf("account = %+v", a)
	}
	if find(report, "pool-1") != nil || find(report, "pool-2") != nil {
		t.Fatalf("expired account was split: %+v", report.Accounts)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 || sent[0] != "Bearer "+poolCodexToken {
		t.Fatalf("sent tokens = %v", sent)
	}
}

func TestExpiredClaudePoolCredentialMergesIntoLocal(t *testing.T) {
	var mu sync.Mutex
	sent := map[string]int{}
	host := newFakeHost(func(method, url string, headers map[string][]string) fakeHTTP {
		mu.Lock()
		sent[headers["Authorization"][0]]++
		mu.Unlock()
		return standardHandler(method, url, headers)
	})
	host.files = []map[string]any{{"auth_index": "1", "provider": "claude"}}
	host.authJSON["1"] = `{"access_token":"synthetic-expired-claude","account_uuid":"` + claudeUUID + `","expired":"2026-10-20T07:00:00Z"}`

	s, _ := newTestService(fakeLocal{
		claude:   credential{token: localClaudeToken},
		codexErr: errNotConfigured,
	}, host, nil)
	report := snap(t, s, true)
	a := find(report, "local-claude")
	if a == nil || a.Status != StatusOK || len(a.MergedPool) != 1 || a.MergedPool[0] != 1 {
		t.Fatalf("account = %+v", report.Accounts)
	}
	if find(report, "pool-1") != nil {
		t.Fatalf("expired credential was split: %+v", report.Accounts)
	}
	mu.Lock()
	defer mu.Unlock()
	if sent["Bearer synthetic-expired-claude"] != 0 {
		t.Fatalf("expired token was sent: %v", sent)
	}
}

func TestAllExpiredCredentialsReportOneErrorCard(t *testing.T) {
	host := newFakeHost(standardHandler)
	host.files = []map[string]any{
		{"auth_index": "1", "provider": "claude"},
		{"auth_index": "2", "provider": "claude"},
		{"auth_index": "3", "provider": "claude"},
	}
	host.authJSON["1"] = `{"access_token":"synthetic-expired-1","account_uuid":"uuid-expired","expired":"2026-10-20T07:00:00Z"}`
	host.authJSON["2"] = `{"access_token":"synthetic-expired-2","account_uuid":"uuid-expired","expired":"2026-10-20T07:00:00Z"}`
	// No stored identity: learning it would require sending the dead token.
	host.authJSON["3"] = `{"access_token":"synthetic-expired-3","expired":"2026-10-20T07:00:00Z"}`

	s, _ := newTestService(fakeLocal{claudeErr: errNotConfigured, codexErr: errNotConfigured}, host, nil)
	report := snap(t, s, true)
	a := find(report, "pool-1")
	if a == nil || a.Status != StatusError || a.Error != ErrTokenExpired || len(a.MergedPool) != 1 || a.MergedPool[0] != 2 {
		t.Fatalf("merged expired account = %+v", report.Accounts)
	}
	if b := find(report, "pool-3"); b == nil || b.Error != ErrTokenExpired {
		t.Fatalf("unidentified expired account = %+v", report.Accounts)
	}
	if find(report, "pool-2") != nil {
		t.Fatalf("expired account was split: %+v", report.Accounts)
	}
	if host.count(methodHTTPDo) != 0 || host.count(claudeProfileURL) != 0 {
		t.Fatalf("expired tokens were sent: %v", host.calls)
	}
}

func TestCodexUnknownResponseKeepsSessionLogFallback(t *testing.T) {
	obs := sessionObservation{
		observedAt: fixedNow.Add(-time.Minute),
		windows:    []Window{{Kind: KindSession, WindowSeconds: 18000, UsedPercent: percentOf(55), ResetsAt: fixedNow.Add(2 * time.Hour).Format(time.RFC3339)}},
	}
	sessions := func(time.Time) (sessionObservation, bool) { return obs, true }
	host := newFakeHost(func(method, url string, headers map[string][]string) fakeHTTP {
		if url == codexUsageURL {
			// HTTP 200 with a body that lacks the documented structure.
			return fakeHTTP{200, `{}`}
		}
		return standardHandler(method, url, headers)
	})
	s, _ := newTestService(fakeLocal{claudeErr: errNotConfigured, codex: credential{token: localCodexToken, accountID: codexAccount}}, host, sessions)
	report := snap(t, s, true)
	a := find(report, "local-codex")
	if a == nil || a.Status != StatusOK || a.Source != SourceSessionLog || a.LiveError != ErrUnknownSchema {
		t.Fatalf("account = %+v", a)
	}
	if len(a.Windows) != 1 || a.Windows[0].UsedPercent == nil || *a.Windows[0].UsedPercent != 55 {
		t.Fatalf("windows = %+v", a.Windows)
	}
}

func TestPoolListFailureKeepsLocalAccounts(t *testing.T) {
	host := newFakeHost(standardHandler)
	host.listErr = true
	s, _ := newTestService(fakeLocal{claudeErr: errNotConfigured, codex: credential{token: localCodexToken, accountID: codexAccount}}, host, nil)
	report := snap(t, s, true)
	if report.PoolStatus != PoolError || find(report, "local-codex").Status != StatusOK {
		t.Fatalf("report = %+v", report)
	}
}

func TestPlainSnapshotStaysOfflineAndCacheTTL(t *testing.T) {
	host := newFakeHost(standardHandler)
	s, c := newTestService(fakeLocal{claudeErr: errNotConfigured, codex: credential{token: localCodexToken, accountID: codexAccount}}, host, nil)

	report := snap(t, s, false)
	if len(report.Accounts) != 0 || report.RefreshedAt != "" || host.count(methodHTTPDo) != 0 || host.count(methodAuthList) != 0 {
		t.Fatalf("plain snapshot touched the host: %+v calls=%v", report, host.calls)
	}

	_ = snap(t, s, true)
	if host.count(codexUsageURL) != 1 {
		t.Fatalf("first refresh calls = %d", host.count(codexUsageURL))
	}
	report = snap(t, s, false)
	if a := find(report, "local-codex"); a == nil || a.Source != SourceCache || report.RefreshedAt == "" {
		t.Fatalf("plain after refresh = %+v", report)
	}

	c.add(4 * time.Minute)
	report = snap(t, s, true)
	if host.count(codexUsageURL) != 1 || find(report, "local-codex").Source != SourceCache {
		t.Fatalf("refresh inside TTL queried upstream: %d", host.count(codexUsageURL))
	}
	c.add(2 * time.Minute)
	report = snap(t, s, true)
	if host.count(codexUsageURL) != 2 || find(report, "local-codex").Source != SourceLive {
		t.Fatalf("refresh after TTL calls = %d", host.count(codexUsageURL))
	}
}

func TestErrorResultsRetryAfterShortTTL(t *testing.T) {
	status := 500
	host := newFakeHost(func(method, url string, headers map[string][]string) fakeHTTP { return fakeHTTP{status, `{}`} })
	s, c := newTestService(fakeLocal{claudeErr: errNotConfigured, codex: credential{token: localCodexToken, accountID: codexAccount}}, host, nil)
	_ = snap(t, s, true)
	_ = snap(t, s, true)
	if host.count(codexUsageURL) != 1 {
		t.Fatalf("error retried inside TTL: %d", host.count(codexUsageURL))
	}
	c.add(errorTTL)
	status = 200
	host.handler = standardHandler
	report := snap(t, s, true)
	if host.count(codexUsageURL) != 2 || find(report, "local-codex").Status != StatusOK {
		t.Fatalf("error not retried: %d", host.count(codexUsageURL))
	}
}

func TestProfileCachedByTokenFingerprint(t *testing.T) {
	host := newFakeHost(standardHandler)
	s, c := newTestService(fakeLocal{claude: credential{token: localClaudeToken}, codexErr: errNotConfigured}, host, nil)
	_ = snap(t, s, true)
	c.add(resultTTL)
	_ = snap(t, s, true)
	if host.count(claudeProfileURL) != 1 || host.count(claudeUsageURL) != 2 {
		t.Fatalf("profile=%d usage=%d", host.count(claudeProfileURL), host.count(claudeUsageURL))
	}
	c.add(profileTTL)
	_ = snap(t, s, true)
	if host.count(claudeProfileURL) != 2 {
		t.Fatalf("profile not refreshed after TTL: %d", host.count(claudeProfileURL))
	}
}

func TestConcurrentRefreshesShareOneRun(t *testing.T) {
	host := newFakeHost(standardHandler)
	host.block = make(chan struct{})
	s, _ := newTestService(fakeLocal{claudeErr: errNotConfigured, codex: credential{token: localCodexToken, accountID: codexAccount}}, host, nil)
	var wg sync.WaitGroup
	statuses := make([]int, 5)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i], _ = s.Snapshot(true)
		}(i)
	}
	deadline := time.Now().Add(2 * time.Second)
	for host.count(codexUsageURL) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(host.block)
	wg.Wait()
	for _, status := range statuses {
		if status != 200 {
			t.Fatalf("statuses = %v", statuses)
		}
	}
	if host.count(codexUsageURL) != 1 {
		t.Fatalf("concurrent refreshes queried %d times", host.count(codexUsageURL))
	}
}

func TestPauseCancelsRefreshAndCloseIsFinal(t *testing.T) {
	host := newFakeHost(standardHandler)
	host.block = make(chan struct{})
	s, _ := newTestService(fakeLocal{claudeErr: errNotConfigured, codex: credential{token: localCodexToken, accountID: codexAccount}}, host, nil)
	done := make(chan int)
	go func() { status, _ := s.Snapshot(true); done <- status }()
	deadline := time.Now().Add(2 * time.Second)
	for host.count(codexUsageURL) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	s.Pause()
	close(host.block)
	if status := <-done; status != 503 {
		t.Fatalf("paused refresh status = %d", status)
	}
	if status, _ := s.Snapshot(false); status != 503 {
		t.Fatalf("paused snapshot status = %d", status)
	}
	s.Resume()
	if status, _ := s.Snapshot(false); status != 200 {
		t.Fatalf("resumed status = %d", status)
	}
	s.Close()
	s.Close()
	s.Resume()
	if status, _ := s.Snapshot(true); status != 503 {
		t.Fatalf("closed status = %d", status)
	}
}

func TestHostDoCancelsOperationOnContext(t *testing.T) {
	host := newFakeHost(standardHandler)
	host.block = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error)
	go func() {
		_, err := HostFunc(host.fn).do(ctx, httpRequest{Method: "GET", URL: codexUsageURL})
		errs <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for host.count(codexUsageURL) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	deadline = time.Now().Add(2 * time.Second)
	for host.count(methodHTTPCancel) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(host.block)
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(host.canceled) != 1 || host.canceled[0] != "7" {
		t.Fatalf("canceled = %v", host.canceled)
	}
	if _, err := HostFunc(nil).do(context.Background(), httpRequest{}); !errors.Is(err, errHostUnavailable) {
		t.Fatalf("nil host err = %v", err)
	}
}

func TestNoHostReportsLocalErrorsOffline(t *testing.T) {
	s, _ := newTestService(fakeLocal{claudeErr: errKeychainDenied, codex: credential{token: localCodexToken}}, nil, nil)
	report := snap(t, s, true)
	if report.PoolStatus != PoolUnavailable || find(report, "local-claude").Error != ErrKeychainDenied || find(report, "local-codex").Error != ErrUpstream {
		t.Fatalf("report = %+v", report.Accounts)
	}
}

// ---- session log fallback ----

func writeRollout(t *testing.T, root string, day time.Time, name string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(root, day.Format("2006"), day.Format("01"), day.Format("02"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func tokenCount(ts time.Time, rateLimits string) string {
	return `{"timestamp":"` + ts.Format(time.RFC3339Nano) + `","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1}},"rate_limits":` + rateLimits + `}}`
}

func TestSessionLogFallback(t *testing.T) {
	root := t.TempDir()
	now := fixedNow
	resetSoon := now.Add(38 * time.Minute).Unix()
	resetLater := now.Add(44 * time.Hour).Unix()
	current := `{"limit_id":"codex","primary":{"used_percent":88,"window_minutes":300,"resets_at":` + itoa(resetSoon) + `},"secondary":{"used_percent":72,"window_minutes":10080,"resets_at":` + itoa(resetLater) + `},"plan_type":"plus"}`
	older := writeRollout(t, root, now.AddDate(0, 0, -1), "rollout-a.jsonl",
		`{"timestamp":"2026-10-19T07:00:00Z","type":"session_meta","payload":{"id":"secret-session"}}`,
		tokenCount(now.Add(-25*time.Hour), `{"primary":{"used_percent":10,"window_minutes":300,"resets_at":`+itoa(resetSoon)+`}}`),
	)
	writeRollout(t, root, now, "rollout-b.jsonl",
		tokenCount(now.Add(-2*time.Hour), current),
		tokenCount(now.Add(-90*time.Minute), `{"limit_id":"codex_other_model","primary":{"used_percent":1,"window_minutes":300}}`),
		tokenCount(now.Add(-80*time.Minute), `null`),
		`{"timestamp":"2026-10-20T07:59:00Z","type":"response_item","payload":{"type":"message"}}`,
		`{"truncated":`,
	)
	// The older file was touched last but holds an older observation.
	if err := os.Chtimes(older, now, now); err != nil {
		t.Fatal(err)
	}
	obs, ok := latestSessionRateLimits(root, now)
	if !ok {
		t.Fatal("no observation")
	}
	if !obs.observedAt.Equal(now.Add(-2*time.Hour)) || obs.plan != "Plus" || len(obs.windows) != 2 {
		t.Fatalf("obs = %+v", obs)
	}
	if obs.windows[0].Kind != KindSession || obs.windows[0].UsedPercent == nil || *obs.windows[0].UsedPercent != 88 || obs.windows[1].Kind != KindWeekly {
		t.Fatalf("windows = %+v", obs.windows)
	}

	// Windows whose reset passed since the observation are flagged.
	later := now.Add(time.Hour)
	expired := expireResetWindows(obs.windows, later)
	if !expired[0].ResetSinceObserved || expired[0].UsedPercent != nil || expired[1].ResetSinceObserved {
		t.Fatalf("expired = %+v", expired)
	}
	// Observations older than the lookback are ignored.
	if _, ok := latestSessionRateLimits(root, now.AddDate(0, 0, 10)); ok {
		t.Fatal("stale observation accepted")
	}
}

func TestSessionLogLegacyRelativeReset(t *testing.T) {
	root := t.TempDir()
	observed := fixedNow.Add(-10 * time.Minute)
	writeRollout(t, root, fixedNow, "rollout-legacy.jsonl",
		tokenCount(observed, `{"primary":{"used_percent":40.5,"window_minutes":300,"resets_in_seconds":1800},"secondary":null}`))
	obs, ok := latestSessionRateLimits(root, fixedNow)
	if !ok || len(obs.windows) != 1 {
		t.Fatalf("obs = %+v ok=%v", obs, ok)
	}
	if got := obs.windows[0].ResetsAt; got != observed.Add(30*time.Minute).UTC().Format(time.RFC3339) {
		t.Fatalf("resetsAt = %s", got)
	}
}

func TestSessionLogTailOnly(t *testing.T) {
	root := t.TempDir()
	padding := strings.Repeat(`{"timestamp":"2026-10-20T07:00:00Z","type":"response_item","payload":{"type":"message","content":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}}`+"\n", 6000)
	head := tokenCount(fixedNow.Add(-time.Hour), `{"primary":{"used_percent":5,"window_minutes":300}}`)
	writeRollout(t, root, fixedNow, "rollout-big.jsonl", head, padding)
	if _, ok := latestSessionRateLimits(root, fixedNow); ok {
		t.Fatal("read beyond the tail limit")
	}
}

func TestResetSessionWindowOmitsUsedPercent(t *testing.T) {
	// The 5-hour window reset after it was logged: the balance is unknown, so
	// the report must not claim 0% used, which the page would render as 100%.
	obs := sessionObservation{
		observedAt: fixedNow.Add(-2 * time.Hour),
		windows:    []Window{{Kind: KindSession, WindowSeconds: 18000, UsedPercent: percentOf(90), ResetsAt: fixedNow.Add(-time.Hour).Format(time.RFC3339)}},
	}
	s, _ := newTestService(fakeLocal{claudeErr: errNotConfigured}, nil, func(time.Time) (sessionObservation, bool) { return obs, true })
	status, body := s.Snapshot(false)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	var report Report
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	a := find(report, "local-codex")
	if a == nil || len(a.Windows) != 1 || !a.Windows[0].ResetSinceObserved || a.Windows[0].UsedPercent != nil {
		t.Fatalf("account = %+v", a)
	}
	if strings.Contains(string(body), "usedPercent") {
		t.Fatalf("unknown balance encoded as a percentage: %s", body)
	}
}

func TestSessionLogReadsResumedOldSession(t *testing.T) {
	root := t.TempDir()
	// A session created a month ago keeps appending into its original
	// directory; the newer observation is what matters.
	path := writeRollout(t, root, fixedNow.AddDate(0, 0, -30), "rollout-resumed.jsonl",
		tokenCount(fixedNow.Add(-5*time.Minute), `{"primary":{"used_percent":12,"window_minutes":300}}`))
	if err := os.Chtimes(path, fixedNow, fixedNow); err != nil {
		t.Fatal(err)
	}
	obs, ok := latestSessionRateLimits(root, fixedNow)
	if !ok {
		t.Fatal("resumed session file was not read")
	}
	if !obs.observedAt.Equal(fixedNow.Add(-5 * time.Minute)) || len(obs.windows) != 1 || obs.windows[0].UsedPercent == nil || *obs.windows[0].UsedPercent != 12 {
		t.Fatalf("obs = %+v", obs)
	}
}

func TestSessionLogMergesIntoReport(t *testing.T) {
	obs := sessionObservation{observedAt: fixedNow.Add(-time.Minute), plan: "Plus", windows: []Window{{Kind: KindSession, WindowSeconds: 18000, UsedPercent: percentOf(50), ResetsAt: fixedNow.Add(time.Hour).Format(time.RFC3339)}}}
	sessions := func(time.Time) (sessionObservation, bool) { return obs, true }

	// Offline, before any refresh: the session log provides the local card.
	s, _ := newTestService(fakeLocal{}, nil, sessions)
	report := snap(t, s, false)
	a := find(report, "local-codex")
	if a == nil || a.Source != SourceSessionLog || a.Status != StatusOK || a.Plan != "Plus" || a.ObservedAt == "" {
		t.Fatalf("session account = %+v", a)
	}

	// Live data older than the session log loses; a failed live query keeps
	// its code as liveError.
	host := newFakeHost(func(method, url string, headers map[string][]string) fakeHTTP { return fakeHTTP{401, `{}`} })
	s, _ = newTestService(fakeLocal{claudeErr: errNotConfigured, codex: credential{token: localCodexToken, accountID: codexAccount}}, host, sessions)
	report = snap(t, s, true)
	a = find(report, "local-codex")
	if a.Source != SourceSessionLog || a.LiveError != ErrTokenExpired || a.Status != StatusOK {
		t.Fatalf("fallback account = %+v", a)
	}

	// Newer live data wins over the session log.
	host = newFakeHost(standardHandler)
	s, _ = newTestService(fakeLocal{claudeErr: errNotConfigured, codex: credential{token: localCodexToken, accountID: codexAccount}}, host, sessions)
	report = snap(t, s, true)
	if a = find(report, "local-codex"); a.Source != SourceLive || a.Windows[0].UsedPercent == nil || *a.Windows[0].UsedPercent != 88 {
		t.Fatalf("live account = %+v", a)
	}
	if report.Accounts[0].ID != "local-claude" || report.Accounts[1].ID != "local-codex" {
		t.Fatalf("order = %+v", report.Accounts)
	}
}

func itoa(v int64) string { b, _ := json.Marshal(v); return string(b) }
