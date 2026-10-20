// Package plugin implements the CPA JSON RPC contract independently of cgo.
package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"cpa-harness-usage/internal/usage"
	"cpa-harness-usage/web"
	"gopkg.in/yaml.v3"
)

const ID = "harness-usage"

const (
	// SchemaVersion is the CPA RPC schema this plugin implements.
	SchemaVersion = 6

	DataPath = "/v0/management/harness-usage/daily"
	PagePath = "/v0/resource/plugins/harness-usage/usage"

	cacheTTL = time.Minute
)

var (
	// Version is the plugin release version shown by CPA. Release builds
	// override it with the tag version via -ldflags.
	Version = "0.0.2"
	// Repository is the project source URL shown by CPA. Release builds pin it
	// via -ldflags so the published artifact names the canonical repository.
	Repository = "https://github.com/Roomen/cpa-harness-usage"
)

var (
	defaultSources = []string{"claude", "codex", "opencode"}
	knownSources   = map[string]struct{}{"claude": {}, "codex": {}, "opencode": {}}
)

type snapshotter interface {
	Snapshot(context.Context) usage.Report
}

type scanCall struct {
	done       chan struct{}
	cancel     context.CancelFunc
	generation uint64
	status     int
	body       []byte
}

type Plugin struct {
	mu         sync.Mutex
	collector  snapshotter
	generation uint64
	cached     []byte
	expires    time.Time
	inflight   *scanCall
	paused     bool
	closed     bool
	baseCtx    context.Context
	cancelBase context.CancelFunc
	now        func() time.Time
}

func New() *Plugin {
	ctx, cancel := context.WithCancel(context.Background())
	return &Plugin{
		collector:  usage.New(usage.Config{Sources: append([]string(nil), defaultSources...)}),
		baseCtx:    ctx,
		cancelBase: cancel,
		now:        time.Now,
	}
}

// Close permanently stops the plugin and cancels any in-flight scan. It is
// idempotent and never waits for a scan, so shutdown cannot block on ccusage.
func (p *Plugin) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.paused = true
	p.cached = nil
	p.expires = time.Time{}
	p.cancelBase()
	if p.inflight != nil {
		p.inflight.cancel()
		p.inflight = nil
	}
	p.mu.Unlock()
}

// quiesce pauses future scans and cancels the current one. A later
// reconfigure resumes the plugin; shutdown still follows on replacement.
func (p *Plugin) quiesce() {
	p.mu.Lock()
	p.paused = true
	p.cached = nil
	p.expires = time.Time{}
	if p.inflight != nil {
		p.inflight.cancel()
		p.inflight = nil
	}
	p.mu.Unlock()
}

func (p *Plugin) Handle(method string, raw []byte) (out []byte) {
	defer func() {
		if recover() != nil {
			out = failure("internal_error")
		}
	}()

	switch method {
	case "plugin.register", "plugin.reconfigure":
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if len(raw) > 0 && json.Unmarshal(raw, &req) != nil {
			return failure("invalid_request")
		}
		cfg, err := parseConfig(req.ConfigYAML)
		if err != nil {
			return failure("invalid_config")
		}
		if !p.applyConfig(cfg) {
			return failure("unavailable")
		}
		return success(registrationResult())

	case "management.register":
		return success(map[string]any{
			"Routes": []map[string]string{
				{"Method": "GET", "Path": "/harness-usage/daily"},
			},
			"Resources": []map[string]string{
				{"Path": "/usage", "Menu": "Harness Usage", "Description": "Local harness daily usage"},
			},
		})

	case "management.handle":
		var req struct {
			Method string              `json:"Method"`
			Path   string              `json:"Path"`
			Query  map[string][]string `json:"Query"`
		}
		if json.Unmarshal(raw, &req) != nil {
			return failure("invalid_request")
		}
		if !strings.EqualFold(req.Method, "GET") {
			return response(405, "application/json; charset=utf-8", []byte(`{"error":"method_not_allowed"}`))
		}
		switch req.Path {
		case PagePath:
			return pageResponse(web.Page)
		case DataPath:
			status, body := p.snapshot(wantsRefresh(req.Query))
			return response(status, "application/json; charset=utf-8", body)
		default:
			return response(404, "application/json; charset=utf-8", []byte(`{"error":"not_found"}`))
		}

	case "plugin.quiesce":
		p.quiesce()
		return success(struct{}{})

	case "plugin.shutdown":
		p.Close()
		return success(struct{}{})

	default:
		return failure("unknown_method")
	}
}

func (p *Plugin) applyConfig(cfg usage.Config) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.collector = usage.New(cfg)
	p.generation++
	p.cached = nil
	p.expires = time.Time{}
	p.paused = false
	if p.inflight != nil {
		// Report the config change immediately: waiters see 503 and new
		// requests start against the new collector instead of joining a scan
		// configured with the previous sources.
		p.inflight.cancel()
		p.inflight = nil
	}
	return true
}

type configYAML struct {
	Binary  *string   `yaml:"binary"`
	Sources *[]string `yaml:"sources"`
}

func parseConfig(raw []byte) (usage.Config, error) {
	var parsed configYAML
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &parsed); err != nil {
			return usage.Config{}, err
		}
	}

	binary := usage.DefaultBinary
	if parsed.Binary != nil {
		binary = strings.TrimSpace(*parsed.Binary)
		if binary == "" || strings.ContainsAny(binary, "\x00\r\n") {
			return usage.Config{}, errInvalidConfig
		}
	}

	sources := append([]string(nil), defaultSources...)
	if parsed.Sources != nil {
		if len(*parsed.Sources) == 0 || len(*parsed.Sources) > len(defaultSources) {
			return usage.Config{}, errInvalidConfig
		}
		sources = make([]string, 0, len(*parsed.Sources))
		seen := make(map[string]struct{}, len(*parsed.Sources))
		for _, source := range *parsed.Sources {
			source = strings.TrimSpace(source)
			if _, ok := knownSources[source]; !ok {
				return usage.Config{}, errInvalidConfig
			}
			if _, duplicate := seen[source]; duplicate {
				return usage.Config{}, errInvalidConfig
			}
			seen[source] = struct{}{}
			sources = append(sources, source)
		}
	}
	return usage.Config{Binary: binary, Sources: sources}, nil
}

var errInvalidConfig = &configError{}

type configError struct{}

func (*configError) Error() string { return "invalid configuration" }

func registrationResult() map[string]any {
	return map[string]any{
		"schema_version": SchemaVersion,
		"metadata": map[string]any{
			"Name":             "Harness Usage",
			"Version":          Version,
			"Author":           "cpa-harness-usage",
			"GitHubRepository": Repository,
			"Logo":             "",
			"ConfigFields": []map[string]any{
				{
					"Name":        "binary",
					"Type":        "string",
					"Default":     usage.DefaultBinary,
					"Description": "Use bundled (default), or an explicit local collector executable.",
				},
				{
					"Name":        "sources",
					"Type":        "array",
					"Default":     append([]string(nil), defaultSources...),
					"Description": "Local harnesses to report: claude, codex, opencode.",
				},
			},
		},
		"capabilities": map[string]bool{"management_api": true},
	}
}

func wantsRefresh(query map[string][]string) bool {
	for _, value := range query["refresh"] {
		if value == "1" || strings.EqualFold(value, "true") {
			return true
		}
	}
	return false
}

// snapshot returns an HTTP status and a JSON body. Concurrent callers share
// one in-flight scan. A refresh skips the cache but still joins a scan that is
// already running, so retries cannot stampede ccusage.
func (p *Plugin) snapshot(refresh bool) (int, []byte) {
	for {
		p.mu.Lock()
		if p.closed || p.paused {
			p.mu.Unlock()
			return 503, []byte(`{"error":"unavailable"}`)
		}
		if !refresh && p.cached != nil && p.now().Before(p.expires) {
			body := append([]byte(nil), p.cached...)
			p.mu.Unlock()
			return 200, body
		}
		if call := p.inflight; call != nil {
			done := call.done
			p.mu.Unlock()
			select {
			case <-done:
				p.mu.Lock()
				status, body := call.status, append([]byte(nil), call.body...)
				// A scan canceled by a configuration change is stale: retry
				// against the new collector instead of surfacing a 503.
				retry := status == 503 && call.generation != p.generation && !p.paused && !p.closed
				p.mu.Unlock()
				if retry {
					refresh = false
					continue
				}
				if body != nil {
					return status, body
				}
				// A completed call always publishes a body; loop defensively.
			case <-p.baseCtx.Done():
				return 503, []byte(`{"error":"unavailable"}`)
			}
			continue
		}

		collector := p.collector
		generation := p.generation
		ctx, cancel := context.WithCancel(p.baseCtx)
		call := &scanCall{done: make(chan struct{}), cancel: cancel, generation: generation}
		p.inflight = call
		p.mu.Unlock()

		report := collector.Snapshot(ctx)
		ctxErr := ctx.Err()
		cancel()

		status := 200
		body, err := json.Marshal(report)
		if err != nil {
			status = 500
			body = []byte(`{"error":"collection_failed"}`)
		}
		if ctxErr != nil {
			status = 503
			body = []byte(`{"error":"unavailable"}`)
		}

		p.mu.Lock()
		if p.inflight == call {
			p.inflight = nil
		}
		if status == 200 && generation == p.generation && !p.closed && !p.paused {
			p.cached = append([]byte(nil), body...)
			p.expires = p.now().Add(cacheTTL)
		}
		call.status = status
		call.body = body
		close(call.done)
		p.mu.Unlock()
		return status, body
	}
}

func pageResponse(body []byte) []byte {
	return responseWithHeaders(200, "text/html; charset=utf-8", body, map[string][]string{
		"Content-Security-Policy": {
			"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; " +
				"img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'",
		},
	})
}

func response(status int, contentType string, body []byte) []byte {
	return responseWithHeaders(status, contentType, body, nil)
}

func responseWithHeaders(status int, contentType string, body []byte, extra map[string][]string) []byte {
	headers := map[string][]string{
		"Content-Type":           {contentType},
		"Cache-Control":          {"no-store"},
		"X-Content-Type-Options": {"nosniff"},
		"Referrer-Policy":        {"no-referrer"},
	}
	for key, values := range extra {
		headers[key] = values
	}
	return success(struct {
		StatusCode int
		Headers    map[string][]string
		Body       []byte
	}{status, headers, body})
}

func success(result any) []byte {
	raw, err := json.Marshal(map[string]any{"ok": true, "result": result})
	if err != nil {
		return failure("encoding_failed")
	}
	return raw
}

func failure(code string) []byte {
	raw, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]string{"code": code, "message": code}})
	return raw
}
