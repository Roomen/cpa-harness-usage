package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cpa-harness-usage/internal/usage"
)

const routeTestPrices = `{"_cloudprice":{"updated_at":"2026-10-19"},"sample":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002}}`

func isolatePricing(t *testing.T, p *Plugin) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prices.json")
	p.mu.Lock()
	p.pricing = usage.NewPricingStore(path)
	p.collector = usage.New(usage.Config{Pricing: p.pricing})
	p.mu.Unlock()
	return path
}

func hostEnvelope(v any) []byte {
	b, _ := json.Marshal(map[string]any{"ok": true, "result": v})
	return b
}

func priceHost(body []byte) func(string, []byte) ([]byte, error) {
	return func(method string, raw []byte) ([]byte, error) {
		switch method {
		case "host.http.operation_open":
			return hostEnvelope(map[string]string{"operation_id": "synthetic-operation"}), nil
		case "host.http.do":
			var req struct {
				Method  string              `json:"method"`
				URL     string              `json:"url"`
				Headers map[string][]string `json:"headers"`
			}
			if json.Unmarshal(raw, &req) != nil || req.Method != "GET" || req.URL != cloudPriceURL {
				return nil, os.ErrInvalid
			}
			if len(req.Headers["Authorization"]) != 0 || len(req.Headers["X-Management-Key"]) != 0 {
				return nil, os.ErrPermission
			}
			return hostEnvelope(map[string]any{"StatusCode": 200, "Body": body}), nil
		default:
			return nil, os.ErrInvalid
		}
	}
}

func TestPricingGETDoesNotCallHostAndRefreshQueryDoesNotDownload(t *testing.T) {
	p := New()
	defer p.Close()
	isolatePricing(t, p)
	calls := 0
	p.SetHost(func(string, []byte) ([]byte, error) { calls++; return nil, os.ErrInvalid })
	for _, q := range []map[string][]string{nil, {"refresh": {"1"}}} {
		res := callManagement(t, p, "GET", PricingPath, q)
		if res.StatusCode != 200 {
			t.Fatalf("GET status = %d, body %s", res.StatusCode, res.Body)
		}
	}
	if calls != 0 {
		t.Fatalf("GET invoked host %d times", calls)
	}
}

func TestPricingPOSTPersistsInvalidatesCacheAndUsesPublicRequest(t *testing.T) {
	p := New()
	defer p.Close()
	path := isolatePricing(t, p)
	p.SetHost(priceHost([]byte(routeTestPrices)))
	p.mu.Lock()
	p.cached = []byte(`{"old":true}`)
	p.expires = time.Now().Add(time.Minute)
	before := p.generation
	p.mu.Unlock()

	res := callManagement(t, p, "POST", PricingPath, nil)
	if res.StatusCode != 200 {
		t.Fatalf("POST status = %d, body %s", res.StatusCode, res.Body)
	}
	var status usage.PricingStatus
	if err := json.Unmarshal(res.Body, &status); err != nil || status.Status != "ready" || status.ModelCount != 1 {
		t.Fatalf("POST status payload = %+v, error %v", status, err)
	}
	p.mu.Lock()
	if p.generation != before+1 || p.cached != nil || !p.expires.IsZero() {
		t.Fatalf("cache state after POST: generation=%d cached=%q expires=%v", p.generation, p.cached, p.expires)
	}
	p.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		t.Fatalf("pricing cache missing: bytes=%d error=%v", len(data), err)
	}
	restarted := usage.NewPricingStore(path)
	if got := restarted.Status(); got.Status != "ready" || got.ModelCount != 1 {
		t.Fatalf("restarted pricing status = %+v", got)
	}
}

func TestPricingFailedUpdatesPreserveStatusAndCache(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
		code int
	}{
		{name: "download", body: []byte("ignored"), code: 502},
		{name: "schema", body: []byte(`{"model":{"input_cost_per_token":1}}`), code: 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New()
			defer p.Close()
			path := isolatePricing(t, p)
			if err := p.pricing.Update([]byte(routeTestPrices)); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			p.SetHost(func(method string, raw []byte) ([]byte, error) {
				if method == "host.http.operation_open" {
					return hostEnvelope(map[string]string{"operation_id": "synthetic"}), nil
				}
				if method == "host.http.do" {
					code := 200
					if tc.code == 502 {
						code = 503
					}
					return hostEnvelope(map[string]any{"StatusCode": code, "Body": tc.body}), nil
				}
				return nil, os.ErrInvalid
			})
			p.mu.Lock()
			p.cached = []byte(`{"keep":true}`)
			p.expires = time.Now().Add(time.Minute)
			generation := p.generation
			p.mu.Unlock()

			res := callManagement(t, p, "POST", PricingPath, nil)
			if res.StatusCode != tc.code {
				t.Fatalf("POST status = %d, want %d (%s)", res.StatusCode, tc.code, res.Body)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatalf("persisted cache changed: error=%v", err)
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.generation != generation || string(p.cached) != `{"keep":true}` || p.expires.IsZero() {
				t.Fatalf("in-memory cache changed: generation=%d cached=%q expires=%v", p.generation, p.cached, p.expires)
			}
			if got := p.pricing.Status(); got.Status != "ready" || got.ModelCount != 1 {
				t.Fatalf("pricing status changed: %+v", got)
			}
		})
	}
}

func TestPricingDuplicatePOSTReturnsConflict(t *testing.T) {
	p := New()
	defer p.Close()
	isolatePricing(t, p)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p.SetHost(func(method string, raw []byte) ([]byte, error) {
		if method == "host.http.operation_open" {
			return hostEnvelope(map[string]string{"operation_id": "synthetic"}), nil
		}
		if method == "host.http.do" {
			once.Do(func() { close(started) })
			<-release
			return hostEnvelope(map[string]any{"StatusCode": 200, "Body": []byte(routeTestPrices)}), nil
		}
		return nil, os.ErrInvalid
	})
	first := make(chan managementResponse, 1)
	go func() { first <- callManagement(t, p, "POST", PricingPath, nil) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first download did not start")
	}
	second := callManagement(t, p, "POST", PricingPath, nil)
	if second.StatusCode != 409 {
		t.Fatalf("duplicate POST status = %d, body %s", second.StatusCode, second.Body)
	}
	close(release)
	if res := <-first; res.StatusCode != 200 {
		t.Fatalf("first POST status = %d, body %s", res.StatusCode, res.Body)
	}
}

func TestPricingQuiesceAndCloseCancelHostOperation(t *testing.T) {
	for _, action := range []string{"quiesce", "close"} {
		t.Run(action, func(t *testing.T) {
			p := New()
			isolatePricing(t, p)
			started, canceled := make(chan struct{}), make(chan struct{})
			var once sync.Once
			p.SetHost(func(method string, raw []byte) ([]byte, error) {
				switch method {
				case "host.http.operation_open":
					return hostEnvelope(map[string]string{"operation_id": "synthetic"}), nil
				case "host.http.do":
					close(started)
					<-canceled
					return hostEnvelope(map[string]any{"StatusCode": 200, "Body": []byte(routeTestPrices)}), nil
				case "host.http.cancel":
					once.Do(func() { close(canceled) })
					return hostEnvelope(map[string]bool{}), nil
				default:
					return nil, os.ErrInvalid
				}
			})
			resultCh := make(chan managementResponse, 1)
			go func() { resultCh <- callManagement(t, p, "POST", PricingPath, nil) }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("download did not enter host operation")
			}
			if action == "quiesce" {
				p.quiesce()
			} else {
				p.Close()
			}
			select {
			case <-canceled:
			case <-time.After(3 * time.Second):
				t.Fatal("host operation was not canceled")
			}
			select {
			case res := <-resultCh:
				if res.StatusCode != 503 {
					t.Fatalf("canceled POST status = %d, body %s", res.StatusCode, res.Body)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("canceled POST did not return")
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.pricing.Status().Status != "empty" || p.generation != 0 || p.cached != nil {
				t.Fatalf("canceled update published state: status=%+v generation=%d cached=%q", p.pricing.Status(), p.generation, p.cached)
			}
		})
	}
}

func TestPricingWrongMethodAndUnavailable(t *testing.T) {
	p := New()
	defer p.Close()
	isolatePricing(t, p)
	if res := callManagement(t, p, "PUT", PricingPath, nil); res.StatusCode != 405 {
		t.Fatalf("PUT status = %d", res.StatusCode)
	}
	p.quiesce()
	if res := callManagement(t, p, "GET", PricingPath, nil); res.StatusCode != 503 {
		t.Fatalf("quiesced GET status = %d", res.StatusCode)
	}
	if res := callManagement(t, p, "POST", PricingPath, nil); res.StatusCode != 503 {
		t.Fatalf("quiesced POST status = %d", res.StatusCode)
	}
}
