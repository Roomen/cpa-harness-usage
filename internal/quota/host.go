package quota

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// HostFunc performs one CPA host callback and returns the raw response
// envelope ({"ok":true,"result":...} or {"ok":false,"error":...}). The plugin
// entry point supplies it from the C host API; tests supply fakes.
type HostFunc func(method string, request []byte) ([]byte, error)

const (
	methodHTTPDo        = "host.http.do"
	methodHTTPOpen      = "host.http.operation_open"
	methodHTTPCancel    = "host.http.cancel"
	methodAuthList      = "host.auth.list"
	methodAuthGet       = "host.auth.get"
	maxResponseBodySize = 1 << 20
)

var (
	errHostUnavailable = errors.New("host callbacks are unavailable")
	errHostFailed      = errors.New("host callback failed")
	errBodyTooLarge    = errors.New("response body too large")
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
}

func (h HostFunc) call(method string, request any) (json.RawMessage, error) {
	if h == nil {
		return nil, errHostUnavailable
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	out, err := h(method, raw)
	if err != nil {
		return nil, errHostFailed
	}
	var env envelope
	if json.Unmarshal(out, &env) != nil || !env.OK {
		return nil, errHostFailed
	}
	return env.Result, nil
}

type httpRequest struct {
	Method  string
	URL     string
	Headers map[string][]string
}

type httpResponse struct {
	StatusCode int
	Body       []byte
}

// do sends a request through host.http.do so CPA applies its configured
// proxy-url. No callback ID is passed: the request is not bound to (or
// logged with) a single management request, and a shared refresh survives
// the browser that triggered it disconnecting. An explicit operation lets the
// context cancel the request through host.http.cancel, since host.http.do
// has no timeout of its own.
func (h HostFunc) do(ctx context.Context, req httpRequest) (httpResponse, error) {
	if h == nil {
		return httpResponse{}, errHostUnavailable
	}
	if err := ctx.Err(); err != nil {
		return httpResponse{}, err
	}
	openRaw, err := h.call(methodHTTPOpen, struct{}{})
	if err != nil {
		return httpResponse{}, err
	}
	var opened struct {
		OperationID string `json:"operation_id"`
	}
	if json.Unmarshal(openRaw, &opened) != nil || strings.TrimSpace(opened.OperationID) == "" {
		return httpResponse{}, errHostFailed
	}
	stop := context.AfterFunc(ctx, func() {
		_, _ = h.call(methodHTTPCancel, map[string]string{"operation_id": opened.OperationID})
	})
	defer stop()

	resultRaw, err := h.call(methodHTTPDo, map[string]any{
		"operation_id": opened.OperationID,
		"method":       req.Method,
		"url":          req.URL,
		"headers":      req.Headers,
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return httpResponse{}, ctxErr
	}
	if err != nil {
		return httpResponse{}, err
	}
	// pluginapi.HTTPResponse has no JSON tags: StatusCode, Headers, Body
	// (base64 bytes).
	var res struct {
		StatusCode int
		Body       []byte
	}
	if json.Unmarshal(resultRaw, &res) != nil {
		return httpResponse{}, errHostFailed
	}
	if len(res.Body) > maxResponseBodySize {
		return httpResponse{}, errBodyTooLarge
	}
	return httpResponse{StatusCode: res.StatusCode, Body: res.Body}, nil
}

// poolEntry is one CPA credential for a supported provider.
type poolEntry struct {
	number    int
	authIndex string
	provider  string
}

// listPool returns CPA claude/codex file credentials ordered by auth index,
// numbered from 1. Disabled and runtime-only (config API key) entries are
// skipped: they have no OAuth token file to read.
func (h HostFunc) listPool() ([]poolEntry, error) {
	raw, err := h.call(methodAuthList, struct{}{})
	if err != nil {
		return nil, err
	}
	var listed struct {
		Files []struct {
			AuthIndex   string `json:"auth_index"`
			Type        string `json:"type"`
			Provider    string `json:"provider"`
			Disabled    bool   `json:"disabled"`
			RuntimeOnly bool   `json:"runtime_only"`
			Status      string `json:"status"`
		} `json:"files"`
	}
	if json.Unmarshal(raw, &listed) != nil {
		return nil, errHostFailed
	}
	entries := make([]poolEntry, 0, len(listed.Files))
	for _, file := range listed.Files {
		provider := strings.ToLower(strings.TrimSpace(file.Provider))
		if provider == "" {
			provider = strings.ToLower(strings.TrimSpace(file.Type))
		}
		if provider != ProviderClaude && provider != ProviderCodex {
			continue
		}
		if file.Disabled || file.RuntimeOnly || strings.EqualFold(file.Status, "disabled") || strings.TrimSpace(file.AuthIndex) == "" {
			continue
		}
		entries = append(entries, poolEntry{authIndex: strings.TrimSpace(file.AuthIndex), provider: provider})
	}
	sort.SliceStable(entries, func(i, j int) bool { return lessAuthIndex(entries[i].authIndex, entries[j].authIndex) })
	for i := range entries {
		entries[i].number = i + 1
	}
	return entries, nil
}

func lessAuthIndex(a, b string) bool {
	an, aErr := strconv.ParseUint(a, 10, 64)
	bn, bErr := strconv.ParseUint(b, 10, 64)
	if aErr == nil && bErr == nil {
		return an < bn
	}
	return a < b
}

// authJSON returns the credential file JSON for an auth index.
func (h HostFunc) authJSON(authIndex string) (json.RawMessage, error) {
	raw, err := h.call(methodAuthGet, map[string]string{"auth_index": authIndex})
	if err != nil {
		return nil, err
	}
	var got struct {
		JSON json.RawMessage `json:"json"`
	}
	if json.Unmarshal(raw, &got) != nil || len(got.JSON) == 0 {
		return nil, errHostFailed
	}
	return got.JSON, nil
}
