package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"cpa-harness-usage/internal/usage"
)

// CloudPrice's documented LiteLLM-compatible price map. Only public prices
// are downloaded; no usage, model selections, or management credentials are sent.
const cloudPriceURL = "https://ai.cloudprice.net/api/v1/litellm_model_prices.json"

func pricingError(code string) []byte {
	body, _ := json.Marshal(map[string]string{"error": code})
	return body
}

func (p *Plugin) pricingRequest(method string) (int, []byte) {
	if !strings.EqualFold(method, "GET") && !strings.EqualFold(method, "POST") {
		return 405, pricingError("method_not_allowed")
	}
	if strings.EqualFold(method, "GET") {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.closed || p.paused {
			return 503, pricingError("unavailable")
		}
		body, _ := json.Marshal(p.pricing.Status())
		return 200, body
	}
	// A second click must not start another download or race a cache replacement.
	if !p.pricingRefreshMu.TryLock() {
		return 409, pricingError("update_in_progress")
	}
	defer p.pricingRefreshMu.Unlock()
	p.mu.Lock()
	if p.closed || p.paused {
		p.mu.Unlock()
		return 503, pricingError("unavailable")
	}
	if p.pricingPersistenceUnavailable {
		p.mu.Unlock()
		return 503, pricingError("cache_unavailable")
	}
	host := p.pricingHost
	ctx, cancel := context.WithTimeout(p.baseCtx, 45*time.Second)
	p.pricingCancel = cancel
	p.mu.Unlock()
	defer func() {
		cancel()
		p.mu.Lock()
		p.pricingCancel = nil
		p.mu.Unlock()
	}()

	data, err := downloadPrices(ctx, host)
	if err != nil {
		if ctx.Err() != nil {
			return 503, pricingError("update_canceled")
		}
		return 502, pricingError("download_failed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.paused || ctx.Err() != nil {
		return 503, pricingError("unavailable")
	}
	if err := p.pricing.Update(data); err != nil {
		return 500, pricingError("price_update_failed")
	}
	// New prices apply to the next scan. Cancel old-price work so it cannot
	// repopulate the one-minute usage cache after the update succeeds.
	p.generation++
	p.cached = nil
	p.expires = time.Time{}
	if p.inflight != nil {
		p.inflight.cancel()
		p.inflight = nil
	}
	body, _ := json.Marshal(p.pricing.Status())
	return 200, body
}

func downloadPrices(ctx context.Context, host func(string, []byte) ([]byte, error)) ([]byte, error) {
	if host == nil {
		return nil, errors.New("host unavailable")
	}
	call := func(method string, request any) (json.RawMessage, error) {
		raw, _ := json.Marshal(request)
		out, err := host(method, raw)
		if err != nil || len(out) > 2*usage.MaxPricingBytes {
			return nil, errors.New("host request failed")
		}
		var env struct {
			OK     bool            `json:"ok"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(out, &env) != nil || !env.OK {
			return nil, errors.New("host request failed")
		}
		return env.Result, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := call("host.http.operation_open", struct{}{})
	if err != nil {
		return nil, err
	}
	var opened struct {
		OperationID string `json:"operation_id"`
	}
	if json.Unmarshal(raw, &opened) != nil || strings.TrimSpace(opened.OperationID) == "" {
		return nil, errors.New("invalid operation")
	}
	stop := context.AfterFunc(ctx, func() {
		_, _ = call("host.http.cancel", map[string]string{"operation_id": opened.OperationID})
	})
	defer stop()
	raw, err = call("host.http.do", map[string]any{
		"operation_id": opened.OperationID,
		"method":       "GET", "url": cloudPriceURL,
		"headers": map[string][]string{"Accept": {"application/json"}, "User-Agent": {"cpa-harness-usage/" + Version}},
	})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	var res struct {
		StatusCode int
		Body       []byte
	}
	if json.Unmarshal(raw, &res) != nil || res.StatusCode != 200 || len(res.Body) > usage.MaxPricingBytes {
		return nil, errors.New("invalid price response")
	}
	return res.Body, nil
}
