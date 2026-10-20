package plugin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"cpa-harness-usage/internal/usage"
	"cpa-harness-usage/web"
)

type fakeCollector struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
	fn      func(context.Context) usage.Report
}

func (f *fakeCollector) Snapshot(ctx context.Context) usage.Report {
	f.mu.Lock()
	f.calls++
	started, release, fn := f.started, f.release, f.fn
	f.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return usage.Report{SchemaVersion: 1, Sources: []usage.SourceReport{{Source: "claude", Status: usage.StatusError, Error: usage.ErrorCanceled}}}
		}
	}
	if fn != nil {
		return fn(ctx)
	}
	return usage.Report{
		SchemaVersion: 1,
		GeneratedAt:   "2026-10-19T00:00:00Z",
		Sources: []usage.SourceReport{{
			Source: "claude",
			Status: usage.StatusOK,
			Daily:  []usage.DailyUsage{{Date: "2026-10-19", InputTokens: 1, OutputTokens: 2, TotalTokens: 3, CostUSD: 0.1}},
		}},
	}
}

func (f *fakeCollector) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func result(t *testing.T, raw []byte) json.RawMessage {
	t.Helper()
	var env struct {
		OK     bool
		Result json.RawMessage
		Error  *struct{ Code string }
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("invalid envelope: %s", raw)
	}
	return env.Result
}

type managementResponse struct {
	StatusCode int
	Headers    map[string][]string
	Body       []byte
}

func callManagement(t *testing.T, p *Plugin, method, path string, query map[string][]string) managementResponse {
	t.Helper()
	req, _ := json.Marshal(map[string]any{"Method": method, "Path": path, "Query": query})
	var res managementResponse
	if err := json.Unmarshal(result(t, p.Handle("management.handle", req)), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func useFake(p *Plugin, fake *fakeCollector) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.collector = fake
	p.cached = nil
	p.expires = time.Time{}
	p.generation++
}

func TestRegistrationContract(t *testing.T) {
	p := New()
	defer p.Close()
	var reg struct {
		SchemaVersion int `json:"schema_version"`
		Metadata      struct {
			Name, Version, Author, GitHubRepository string
			ConfigFields                            []struct{ Name, Type string }
		}
		Capabilities map[string]bool
	}
	if err := json.Unmarshal(result(t, p.Handle("plugin.register", []byte(`{}`))), &reg); err != nil {
		t.Fatal(err)
	}
	if reg.SchemaVersion != SchemaVersion {
		t.Fatalf("schema version = %d", reg.SchemaVersion)
	}
	if reg.Metadata.Name == "" || reg.Metadata.Version != Version || reg.Metadata.Author == "" || reg.Metadata.GitHubRepository == "" {
		t.Fatalf("missing required metadata: %+v", reg.Metadata)
	}
	if !reg.Capabilities["management_api"] || len(reg.Capabilities) != 1 {
		t.Fatalf("capabilities = %+v", reg.Capabilities)
	}
	fields := reg.Metadata.ConfigFields
	if len(fields) != 3 {
		t.Fatalf("config fields = %+v", fields)
	}
	if fields[0].Name != "binary" || fields[1].Name != "sources" || fields[2].Name != "omp_path" {
		t.Fatalf("unexpected config fields: %+v", fields)
	}
}

func TestManagementRoutesAndStaticPageBoundary(t *testing.T) {
	p := New()
	defer p.Close()
	var routes struct {
		Routes    []struct{ Method, Path string }
		Resources []struct{ Path, Menu string }
	}
	if err := json.Unmarshal(result(t, p.Handle("management.register", nil)), &routes); err != nil {
		t.Fatal(err)
	}
	if len(routes.Routes) != 1 || routes.Routes[0].Method != "GET" || routes.Routes[0].Path != "/harness-usage/daily" {
		t.Fatalf("routes = %+v", routes.Routes)
	}
	if len(routes.Resources) != 1 || routes.Resources[0].Path != "/usage" || routes.Resources[0].Menu == "" {
		t.Fatalf("resources = %+v", routes.Resources)
	}

	page := callManagement(t, p, "GET", PagePath, nil)
	if page.StatusCode != 200 {
		t.Fatalf("page status = %d", page.StatusCode)
	}
	body := string(page.Body)
	for _, want := range []string{`id="auth-form"`, `id="dashboard"`, "Authorization", "prefers-color-scheme", "cli-proxy-language"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing %q", want)
		}
	}
	for _, rejected := range []string{"ccusage", "localStorage.setItem('management", "?key=", `src="http`, `href="http`} {
		if strings.Contains(body, rejected) {
			t.Fatalf("page leaked implementation or secret handling: %q", rejected)
		}
	}
	// The page shell is public, but the management response still forbids
	// framing, third-party code, and secret-bearing referrers.
	if got := strings.Join(page.Headers["Content-Security-Policy"], ""); !strings.Contains(got, "connect-src 'self'") {
		t.Fatalf("CSP = %q", got)
	}
	if got := strings.Join(page.Headers["Cache-Control"], ""); got != "no-store" {
		t.Fatalf("cache control = %q", got)
	}

	if got := callManagement(t, p, "GET", PagePath+"/daily", nil); got.StatusCode != 404 {
		t.Fatalf("unexpected path status = %d", got.StatusCode)
	}
	if got := callManagement(t, p, "GET", "/harness-usage/daily", nil); got.StatusCode != 404 {
		t.Fatalf("management path is not a resource: %d", got.StatusCode)
	}
	if got := callManagement(t, p, "POST", DataPath, nil); got.StatusCode != 405 {
		t.Fatalf("method status = %d", got.StatusCode)
	}
}

func TestDataEndpointUsesSafeReportAndNoStore(t *testing.T) {
	p := New()
	defer p.Close()
	fake := &fakeCollector{}
	useFake(p, fake)
	res := callManagement(t, p, "GET", DataPath, nil)
	if res.StatusCode != 200 {
		t.Fatalf("status = %d body=%s", res.StatusCode, res.Body)
	}
	if got := strings.Join(res.Headers["Cache-Control"], ""); got != "no-store" {
		t.Fatalf("cache control = %q", got)
	}
	var report usage.Report
	if err := json.Unmarshal(res.Body, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Sources) != 1 || report.Sources[0].Source != "claude" {
		t.Fatalf("report = %+v", report)
	}
	for _, private := range []string{"ccusage", "/Users/", "private-model", "session"} {
		if strings.Contains(string(res.Body), private) {
			t.Fatalf("data leaked %q: %s", private, res.Body)
		}
	}
}

func TestCacheAndRefresh(t *testing.T) {
	p := New()
	defer p.Close()
	fake := &fakeCollector{}
	useFake(p, fake)

	if status, _ := p.snapshot(false); status != 200 {
		t.Fatalf("status = %d", status)
	}
	if status, _ := p.snapshot(false); status != 200 {
		t.Fatalf("status = %d", status)
	}
	if got := fake.callCount(); got != 1 {
		t.Fatalf("cache calls = %d", got)
	}
	if status, _ := p.snapshot(true); status != 200 {
		t.Fatalf("refresh status = %d", status)
	}
	if got := fake.callCount(); got != 2 {
		t.Fatalf("refresh calls = %d", got)
	}
	if res := callManagement(t, p, "GET", DataPath, map[string][]string{"refresh": {"1"}}); res.StatusCode != 200 {
		t.Fatalf("http refresh status = %d", res.StatusCode)
	}
	if got := fake.callCount(); got != 3 {
		t.Fatalf("http refresh calls = %d", got)
	}
}

func TestConcurrentSnapshotsShareOneScan(t *testing.T) {
	p := New()
	defer p.Close()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	fake := &fakeCollector{started: started, release: release}
	useFake(p, fake)

	const callers = 8
	results := make(chan int, callers)
	for range callers {
		go func() {
			status, _ := p.snapshot(false)
			results <- status
		}()
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("scan did not start")
	}
	close(release)
	for range callers {
		select {
		case status := <-results:
			if status != 200 {
				t.Fatalf("status = %d", status)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("caller blocked")
		}
	}
	if got := fake.callCount(); got != 1 {
		t.Fatalf("concurrent scans = %d", got)
	}
}

func TestReconfigureDoesNotWaitForScanAndInvalidatesCache(t *testing.T) {
	p := New()
	defer p.Close()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	fake := &fakeCollector{started: started, release: release}
	useFake(p, fake)

	scanDone := make(chan int, 1)
	go func() {
		status, _ := p.snapshot(false)
		scanDone <- status
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("scan did not start")
	}

	reconfigured := make(chan bool, 1)
	go func() {
		req, _ := json.Marshal(map[string][]byte{"config_yaml": []byte("sources: [codex]\nbinary: ccusage\n")})
		env := p.Handle("plugin.reconfigure", req)
		reconfigured <- strings.Contains(string(env), `"ok":true`)
	}()
	select {
	case ok := <-reconfigured:
		if !ok {
			t.Fatal("reconfigure failed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reconfigure blocked on the running scan")
	}

	close(release)
	select {
	case status := <-scanDone:
		if status != 503 {
			t.Fatalf("canceled scan status = %d", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old scan did not finish")
	}

	// The old scan is canceled and detached, so it is never cached and the
	// next request starts a scan with the new collector immediately.
	p.mu.Lock()
	if p.cached != nil {
		p.mu.Unlock()
		t.Fatal("stale scan was cached")
	}
	next := &fakeCollector{}
	p.collector = next
	p.mu.Unlock()
	if status, _ := p.snapshot(false); status != 200 {
		t.Fatalf("new snapshot status = %d", status)
	}
	if got := next.callCount(); got != 1 {
		t.Fatalf("new collector calls = %d", got)
	}
}

func TestQuiesceCancelsAndReconfigureResumes(t *testing.T) {
	p := New()
	defer p.Close()
	started := make(chan struct{}, 1)
	fake := &fakeCollector{started: started, release: make(chan struct{})}
	useFake(p, fake)

	status := make(chan int, 1)
	go func() {
		value, _ := p.snapshot(false)
		status <- value
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("scan did not start")
	}
	if !strings.Contains(string(p.Handle("plugin.quiesce", nil)), `"ok":true`) {
		t.Fatal("quiesce failed")
	}
	select {
	case value := <-status:
		if value != 503 {
			t.Fatalf("quiesced scan status = %d", value)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quiesce did not cancel the scan")
	}
	if value, _ := p.snapshot(false); value != 503 {
		t.Fatalf("paused snapshot status = %d", value)
	}

	req, _ := json.Marshal(map[string][]byte{"config_yaml": []byte("sources: [claude]\n")})
	if !strings.Contains(string(p.Handle("plugin.reconfigure", req)), `"ok":true`) {
		t.Fatal("reconfigure after quiesce failed")
	}
	p.mu.Lock()
	p.collector = &fakeCollector{}
	p.mu.Unlock()
	if value, _ := p.snapshot(false); value != 200 {
		t.Fatalf("resumed snapshot status = %d", value)
	}
}

func TestCloseCancelsScanAndIsIdempotent(t *testing.T) {
	p := New()
	started := make(chan struct{}, 1)
	useFake(p, &fakeCollector{started: started, release: make(chan struct{})})
	status := make(chan int, 1)
	go func() {
		value, _ := p.snapshot(false)
		status <- value
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("scan did not start")
	}
	p.Close()
	p.Close()
	select {
	case value := <-status:
		if value != 503 {
			t.Fatalf("closed scan status = %d", value)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not cancel the scan")
	}
	if value, _ := p.snapshot(false); value != 503 {
		t.Fatalf("closed snapshot status = %d", value)
	}
	if !strings.Contains(string(p.Handle("plugin.shutdown", nil)), `"ok":true`) {
		t.Fatal("shutdown after close failed")
	}
}

func TestInvalidReconfigurePreservesCollectorAndCache(t *testing.T) {
	p := New()
	defer p.Close()
	fake := &fakeCollector{}
	useFake(p, fake)
	if status, _ := p.snapshot(false); status != 200 {
		t.Fatalf("status = %d", status)
	}
	p.mu.Lock()
	collector, generation, cached := p.collector, p.generation, append([]byte(nil), p.cached...)
	p.mu.Unlock()

	for _, yaml := range []string{
		"sources: [codex, codex]",
		"sources: [unsupported]",
		"sources: []",
		"sources: [claude, codex, opencode, claude]",
		"binary: ''",
		"binary: \"bad\\npath\"",
		"sources: not-a-list",
		"omp_path: \"bad\\npath\"",
	} {
		req, _ := json.Marshal(map[string][]byte{"config_yaml": []byte(yaml)})
		if !strings.Contains(string(p.Handle("plugin.reconfigure", req)), `"invalid_config"`) {
			t.Fatalf("accepted %q", yaml)
		}
		p.mu.Lock()
		same := p.collector == collector && p.generation == generation && string(p.cached) == string(cached)
		p.mu.Unlock()
		if !same {
			t.Fatalf("invalid config changed state: %q", yaml)
		}
	}
}

func TestReconfigureAfterShutdownIsRejected(t *testing.T) {
	p := New()
	p.Close()
	req, _ := json.Marshal(map[string][]byte{"config_yaml": []byte("sources: [claude]\n")})
	if !strings.Contains(string(p.Handle("plugin.reconfigure", req)), `"unavailable"`) {
		t.Fatal("closed plugin accepted configuration")
	}
}

func TestUnknownMethodAndMalformedRequest(t *testing.T) {
	p := New()
	defer p.Close()
	if !strings.Contains(string(p.Handle("nope", nil)), `"unknown_method"`) {
		t.Fatal("unknown method accepted")
	}
	if !strings.Contains(string(p.Handle("management.handle", []byte(`{`))), `"invalid_request"`) {
		t.Fatal("malformed request accepted")
	}
	if !strings.Contains(string(p.Handle("plugin.register", []byte(`{`))), `"invalid_request"`) {
		t.Fatal("malformed lifecycle request accepted")
	}
}

func TestParseConfigDefaultsAndValidation(t *testing.T) {
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Binary != usage.DefaultBinary || !reflect.DeepEqual(cfg.Sources, usage.KnownSources) {
		t.Fatalf("defaults = %+v", cfg)
	}

	cfg, err = parseConfig([]byte("binary: /opt/ccusage\nsources: [codex]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Binary != "/opt/ccusage" || len(cfg.Sources) != 1 || cfg.Sources[0] != "codex" {
		t.Fatalf("custom = %+v", cfg)
	}

	cfg, err = parseConfig([]byte("omp_path: ~/.omp/agent/sessions\nsources: [omp]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OMPPath != "~/.omp/agent/sessions" || len(cfg.Sources) != 1 || cfg.Sources[0] != "omp" {
		t.Fatalf("omp config = %+v", cfg)
	}

	cfg, err = parseConfig([]byte("unknown_field: keep-me\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Binary != usage.DefaultBinary || !reflect.DeepEqual(cfg.Sources, usage.KnownSources) {
		t.Fatalf("unknown field changed defaults: %+v", cfg)
	}
}

func TestPageNeverPersistsOrLogsSecrets(t *testing.T) {
	page := string(web.Page)
	for _, forbidden := range []string{"localStorage.setItem", "sessionStorage.setItem", "console.log", "document.cookie", "?key=", "innerHTML = token", "ccusage"} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("page contains %q", forbidden)
		}
	}
	if !strings.Contains(page, "Authorization: \"Bearer \" + state.key") {
		t.Fatal("page does not send the key via the Authorization header")
	}
}
