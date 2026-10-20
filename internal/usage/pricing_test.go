package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"testing"
)

func TestEstimateCodexGpt5StandardRatesExcludeSeparateReasoning(t *testing.T) {
	rows := parsePricedFixture(t, "codex", `{"date":"2026-10-19","inputTokens":270,"outputTokens":125,"cacheCreationTokens":0,"cacheReadTokens":30,"reasoningOutputTokens":5,"totalTokens":430,"costUSD":0,"models":{"gpt-5":{"inputTokens":270,"outputTokens":125,"cacheCreationTokens":0,"cacheReadTokens":30,"reasoningOutputTokens":5,"totalTokens":430}}}`)
	const want = 0.00159125 // 270*1.25 + 30*0.125 + 125*10, per million tokens.
	if math.Abs(rows[0].CostUSD-want) > 1e-12 {
		t.Fatalf("daily cost = %.12f, want %.12f", rows[0].CostUSD, want)
	}
	model := rows[0].Models[0]
	if model.CostUSD == nil || math.Abs(*model.CostUSD-want) > 1e-12 {
		t.Fatalf("model cost = %v, want %.12f", model.CostUSD, want)
	}
	if !model.CostEstimated || model.MissingPricing || rows[0].MissingPricing {
		t.Fatalf("pricing flags: model=%+v day=%+v", model, rows[0])
	}
}

func TestLookupDeepSeekExplicitAliasesAndQualifiedRates(t *testing.T) {
	plain, ok := lookupModelPrice("deepseek-4.1-flash")
	if !ok || plain.Cost.Input != 0.31 || plain.Cost.Output != 1.23 {
		t.Fatalf("unqualified alias price = %+v, found=%v", plain, ok)
	}
	qualified, ok := lookupModelPrice("deepseek/deepseek-4.1-flash")
	if !ok || qualified.Cost.Input != 0.3 || qualified.Cost.Output != 1.2 || qualified.Cost.CacheRead == nil || *qualified.Cost.CacheRead != 0.006 || qualified.Cost.CacheWrite == nil || *qualified.Cost.CacheWrite != 0.3 {
		t.Fatalf("qualified alias price = %+v, found=%v", qualified, ok)
	}
	if plain.Cost.Input == qualified.Cost.Input || plain.Cost.Output == qualified.Cost.Output {
		t.Fatal("qualified and unqualified DeepSeek entries collapsed to one provider rate")
	}
}

func TestLookupDoesNotFuzzyMatchUnknownOrProviderQualifiedNames(t *testing.T) {
	for _, name := range []string{"gpt5", "new-gpt-5", "unsupported/gpt-5", "provider/deepseek-4.1-flash"} {
		if price, ok := lookupModelPrice(name); ok {
			t.Errorf("lookupModelPrice(%q) = %+v, want no match", name, price)
		}
	}
}

func TestApplyPricingPreservesPositiveModelCost(t *testing.T) {
	modelCost := 0.42
	rows := []DailyUsage{{CostUSD: 0.5, TotalTokens: 10, Models: []ModelUsage{{Model: "gpt-5", TotalTokens: 10, InputTokens: 10, CostUSD: &modelCost}}}}
	applyPricing(rows, "claude")
	if rows[0].Models[0].CostUSD == nil || *rows[0].Models[0].CostUSD != modelCost {
		t.Fatalf("model cost = %v, want preserved %.2f", rows[0].Models[0].CostUSD, modelCost)
	}
	if rows[0].Models[0].CostEstimated {
		t.Fatal("recorded positive model cost marked estimated")
	}
}

func TestSingleModelUsesPositiveReportedDailyCost(t *testing.T) {
	rows := parsePricedFixture(t, "claude", `{"date":"2026-10-19","inputTokens":100,"outputTokens":20,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":120,"totalCost":0.75,"modelBreakdowns":[{"modelName":"gpt-5","inputTokens":100,"outputTokens":20,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":120}]}`)
	if rows[0].CostUSD != 0.75 || rows[0].Models[0].CostUSD == nil || *rows[0].Models[0].CostUSD != 0.75 {
		t.Fatalf("reported daily cost not used: %+v", rows[0])
	}
	if rows[0].Models[0].CostEstimated {
		t.Fatal("single-model reported cost marked estimated")
	}
}

func TestMultiModelCodexKeepsReportedDailyCostAndMarksEstimates(t *testing.T) {
	rows := parsePricedFixture(t, "codex", `{"date":"2026-10-19","inputTokens":40,"outputTokens":10,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":50,"costUSD":0.123,"models":{"gpt-5":{"inputTokens":20,"outputTokens":5,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":25},"gpt-5-codex":{"inputTokens":20,"outputTokens":5,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":25}}}`)
	if rows[0].CostUSD != 0.123 {
		t.Fatalf("daily Codex cost = %.12f, want authoritative 0.123", rows[0].CostUSD)
	}
	for _, model := range rows[0].Models {
		if model.CostUSD == nil || !model.CostEstimated || model.MissingPricing {
			t.Errorf("model %q flags/cost = %+v", model.Model, model)
		}
	}
}

func TestMissingCodexAliasModelRepairsPartialReportedCost(t *testing.T) {
	rows := parsePricedFixture(t, "codex", `{"date":"2026-10-19","inputTokens":200,"outputTokens":10,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":210,"costUSD":0.00015,"models":{"gpt-5":{"inputTokens":100,"outputTokens":0,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":100},"deepseek-4.1-flash":{"inputTokens":100,"outputTokens":10,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":110,"missingPricing":true}}}`)
	wantDeepSeek := (100*0.31 + 10*1.23) / 1e6
	wantGPT5 := 100 * 1.25 / 1e6
	wantDay := 0.00015 + wantDeepSeek
	if math.Abs(rows[0].CostUSD-wantDay) > 1e-12 || !rows[0].CostEstimated || rows[0].MissingPricing {
		t.Fatalf("repaired day = %+v, want reported cost plus alias repair %.12f", rows[0], wantDay)
	}
	var alias *ModelUsage
	var gpt5 *ModelUsage
	for i := range rows[0].Models {
		switch rows[0].Models[i].Model {
		case "deepseek-4.1-flash":
			alias = &rows[0].Models[i]
		case "gpt-5":
			gpt5 = &rows[0].Models[i]
		}
	}
	if alias == nil || alias.CostUSD == nil || math.Abs(*alias.CostUSD-wantDeepSeek) > 1e-12 || !alias.CostEstimated || alias.MissingPricing {
		t.Fatalf("alias model was not repaired: %+v", alias)
	}
	if gpt5 == nil || gpt5.CostUSD == nil || math.Abs(*gpt5.CostUSD-wantGPT5) > 1e-12 || !gpt5.CostEstimated || gpt5.MissingPricing {
		t.Fatalf("unreported model cost was not estimated: %+v", gpt5)
	}
}

func TestPricingFallbackAcrossKnownSources(t *testing.T) {
	for _, source := range KnownSources {
		t.Run(source, func(t *testing.T) {
			row := `{"date":"2026-10-19","inputTokens":10,"outputTokens":2,"cacheCreationTokens":0,"cacheReadTokens":1,"totalTokens":13,"costUSD":0,"totalCost":0,"modelBreakdowns":[{"modelName":"gpt-5","inputTokens":10,"outputTokens":2,"cacheCreationTokens":0,"cacheReadTokens":1,"totalTokens":13}]}`
			rows := parsePricedFixture(t, source, row)
			if len(rows) != 1 || len(rows[0].Models) != 1 || rows[0].Models[0].CostUSD == nil || !rows[0].Models[0].CostEstimated || rows[0].MissingPricing {
				t.Fatalf("pricing fallback result = %+v", rows)
			}
		})
	}
}

func TestMissingPricingRemainsUnknownForPartialAndUnpricedRows(t *testing.T) {
	t.Run("unpriced model", func(t *testing.T) {
		rows := parsePricedFixture(t, "codex", `{"date":"2026-10-19","inputTokens":10,"outputTokens":0,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":10,"costUSD":0,"models":{"unknown-provider/model-x":{"inputTokens":10,"outputTokens":0,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":10}}}`)
		if !rows[0].MissingPricing || !rows[0].Models[0].MissingPricing || rows[0].Models[0].CostUSD != nil {
			t.Fatalf("unknown model pricing not retained: %+v", rows[0])
		}
	})
	t.Run("partial model coverage", func(t *testing.T) {
		rows := parsePricedFixture(t, "codex", `{"date":"2026-10-19","inputTokens":20,"outputTokens":2,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":30,"costUSD":0,"models":{"gpt-5":{"inputTokens":10,"outputTokens":1,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":11}}}`)
		if !rows[0].MissingPricing || rows[0].Models[0].CostUSD == nil {
			t.Fatalf("partial coverage should retain estimated model cost but flag day unknown: %+v", rows[0])
		}
	})
	t.Run("cache rate is not inferred", func(t *testing.T) {
		rows := parsePricedFixture(t, "codex", `{"date":"2026-10-19","inputTokens":0,"outputTokens":0,"cacheCreationTokens":1,"cacheReadTokens":0,"totalTokens":1,"costUSD":0,"models":{"gpt-5":{"inputTokens":0,"outputTokens":0,"cacheCreationTokens":1,"cacheReadTokens":0,"totalTokens":1}}}`)
		if !rows[0].MissingPricing || !rows[0].Models[0].MissingPricing || rows[0].Models[0].CostUSD != nil {
			t.Fatalf("missing cache-write rate should remain unknown: %+v", rows[0])
		}
	})
}

func TestBundledPricingSnapshotMatchesManifest(t *testing.T) {
	data, err := os.ReadFile("pricingdata/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		SnapshotSHA256 string `json:"snapshotSHA256"`
		Models         int    `json:"models"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(pricingJSON)
	if got := hex.EncodeToString(sum[:]); got != manifest.SnapshotSHA256 {
		t.Fatalf("snapshot sha256 = %s, want %s", got, manifest.SnapshotSHA256)
	}
	if len(modelPrices) != manifest.Models {
		t.Fatalf("snapshot model count = %d, want %d", len(modelPrices), manifest.Models)
	}
	for name, price := range modelPrices {
		rates := []float64{price.Cost.Input, price.Cost.Output}
		if price.Cost.CacheRead != nil {
			rates = append(rates, *price.Cost.CacheRead)
		}
		if price.Cost.CacheWrite != nil {
			rates = append(rates, *price.Cost.CacheWrite)
		}
		for _, rate := range rates {
			if rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
				t.Errorf("model %q has invalid rate %v", name, rate)
			}
		}
	}
}

func parsePricedFixture(t *testing.T, source, row string) []DailyUsage {
	t.Helper()
	data := []byte(`{"daily":[` + row + `],"totals":{}}`)
	rows, err := parseSourceDaily(data, source)
	if err != nil {
		t.Fatalf("parseSourceDaily(%q): %v", source, err)
	}
	return rows
}
