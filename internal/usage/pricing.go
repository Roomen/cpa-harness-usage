package usage

import (
	_ "embed"
	"encoding/json"
	"strings"
)

// Prices are USD per million tokens, pinned to the same ccusage release as
// the collector. No network access or harness history is needed for lookup.
//
//go:embed pricingdata/models-dev.json
var pricingJSON []byte

type modelPrice struct {
	Cost struct {
		Input      float64  `json:"input"`
		Output     float64  `json:"output"`
		CacheRead  *float64 `json:"cache_read"`
		CacheWrite *float64 `json:"cache_write"`
	} `json:"cost"`
}

var modelPrices = func() map[string]modelPrice {
	var prices map[string]modelPrice
	if err := json.Unmarshal(pricingJSON, &prices); err != nil {
		panic("invalid bundled model pricing")
	}
	return prices
}()

func lookupModelPrice(name string) (modelPrice, bool) {
	// Adapter tags are not provider identities. Provider-qualified names are
	// kept intact: different providers can charge different rates for a model.
	for _, prefix := range []string{"[pi] ", "[omp] "} {
		name = strings.TrimPrefix(name, prefix)
	}
	if price, ok := modelPrices[name]; ok {
		return price, true
	}
	// models.dev writes dated Claude releases as model@YYYYMMDD, while
	// Anthropic logs use model-YYYYMMDD. Require that exact dated catalog entry.
	if strings.HasPrefix(name, "claude-") && len(name) > 9 && name[len(name)-9] == '-' {
		date := name[len(name)-8:]
		digits := true
		for _, ch := range date {
			digits = digits && ch >= '0' && ch <= '9'
		}
		if digits {
			if price, ok := modelPrices[name[:len(name)-9]+"@"+date]; ok {
				return price, true
			}
		}
	}
	// Explicit spelling aliases only; never fuzzy-match a new model or silently
	// strip its provider and borrow a different channel's price.
	aliases := map[string]string{
		"deepseek-4.1-flash":          "deepseek-v4.1-flash",
		"deepseek/deepseek-4.1-flash": "deepseek/deepseek-v4.1-flash",
	}
	price, ok := modelPrices[aliases[name]]
	return price, ok
}

func estimateModelCost(model ModelUsage) (float64, bool) {
	price, ok := lookupModelPrice(model.Model)
	if !ok {
		return 0, false
	}
	// Do not invent discounts or write premiums when this model's snapshot
	// has no published cache rate for a counter that is actually present.
	if (model.CacheReadTokens > 0 && price.Cost.CacheRead == nil) ||
		(model.CacheCreationTokens > 0 && price.Cost.CacheWrite == nil) {
		return 0, false
	}
	var read, write float64
	if price.Cost.CacheRead != nil {
		read = *price.Cost.CacheRead
	}
	if price.Cost.CacheWrite != nil {
		write = *price.Cost.CacheWrite
	}
	// These counters are normalized into non-overlapping components by the
	// adapter. Match ccusage's Codex pricing: reasoning is not charged again.
	// Daily aggregates cannot reconstruct request-level context tiers or speed.
	cost := (float64(model.InputTokens)*price.Cost.Input +
		float64(model.OutputTokens)*price.Cost.Output +
		float64(model.CacheReadTokens)*read + float64(model.CacheCreationTokens)*write) / 1e6
	return cost, true
}

func applyPricing(rows []DailyUsage, source string) {
	for i := range rows {
		row := &rows[i]
		var total uint64
		for _, model := range row.Models {
			total += model.TotalTokens // parseDaily already checked for overflow.
		}
		complete := total == row.TotalTokens
		originalMissing := row.MissingPricing
		row.MissingPricing = originalMissing
		var sum, repaired, codexRepair float64
		for j := range row.Models {
			model := &row.Models[j]
			oldCost := model.CostUSD
			wasMissing := model.MissingPricing
			if oldCost != nil && *oldCost > 0 && !model.MissingPricing {
				sum += *oldCost
				continue
			}
			// A complete single-model day needs no allocation or rate estimate.
			if len(row.Models) == 1 && complete && row.CostUSD > 0 && !originalMissing && !model.MissingPricing {
				value := row.CostUSD
				model.CostUSD = &value
				model.CostEstimated = false
				sum += value
				continue
			}
			value, ok := estimateModelCost(*model)
			if model.TotalTokens == 0 {
				value, ok = 0, true
			}
			if !ok {
				// A positive day total remains usable even when its model split
				// cannot be priced, unless upstream explicitly flagged missing data.
				model.CostUSD = nil
				if model.MissingPricing || oldCost != nil || row.CostUSD == 0 {
					row.MissingPricing = true
				}
				model.MissingPricing = true
				continue
			}
			model.CostUSD = &value
			model.MissingPricing = false
			model.CostEstimated = model.TotalTokens > 0
			sum += value
			// Codex explicitly flags models excluded from the daily price. Add
			// just those recovered costs, preserving speed/context pricing already
			// included for the other models. Ordinary absent costs are included.
			if source == "codex" && wasMissing {
				codexRepair += value
			} else if wasMissing || (source != "codex" && oldCost != nil && *oldCost == 0) {
				repaired += value
			}
		}
		if complete && len(row.Models) > 0 {
			allPriced := true
			for _, model := range row.Models {
				allPriced = allPriced && !model.MissingPricing
			}
			if allPriced {
				row.MissingPricing = false
			}
		}
		if row.CostUSD == 0 {
			row.CostUSD = sum
			for _, model := range row.Models {
				row.CostEstimated = row.CostEstimated || model.CostEstimated
			}
			if !complete {
				row.MissingPricing = true
			}
		} else if codexRepair > 0 {
			row.CostUSD += codexRepair
			row.CostEstimated = true
		} else if repaired > 0 && complete {
			// A reported total can already include an amount absent from the
			// model breakdown. Do not add the same charge twice.
			if sum > row.CostUSD {
				row.CostUSD = sum
				row.CostEstimated = true
			}
		}
	}
}
