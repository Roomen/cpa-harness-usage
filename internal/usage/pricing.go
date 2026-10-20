package usage

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const MaxPricingBytes = 32 << 20
const maxPricingModels = 100_000
const maxPricingModelName = 1024

type modelPrice struct {
	Input      float64  `json:"input_cost_per_token"`
	Output     float64  `json:"output_cost_per_token"`
	CacheRead  *float64 `json:"cache_read_input_token_cost,omitempty"`
	CacheWrite *float64 `json:"cache_creation_input_token_cost,omitempty"`
}

type PricingStatus struct {
	UpstreamUpdatedAt string `json:"upstreamUpdatedAt,omitempty"`
	Source            string `json:"source"`
	Status            string `json:"status"`
	UpdatedAt         string `json:"updatedAt,omitempty"`
	ModelCount        int    `json:"modelCount"`
}

type pricingCache struct {
	UpstreamUpdatedAt string                `json:"upstreamUpdatedAt,omitempty"`
	Source            string                `json:"source"`
	UpdatedAt         string                `json:"updatedAt"`
	Models            map[string]modelPrice `json:"models"`
}

// PricingStore keeps immutable pricing snapshots. Only explicit Update calls
// change the cache; construction and usage collection never access the network.
type PricingStore struct {
	mu        sync.RWMutex
	updateMu  sync.Mutex
	cachePath string
	cache     pricingCache
	lower     map[string]string
}

func DefaultPricingCachePath() string {
	root, err := os.UserCacheDir()
	if err != nil || root == "" {
		return ""
	}
	return filepath.Join(root, "cpa-harness-usage", "pricing", "cloudprice.json")
}

func NewPricingStore(cachePath string) *PricingStore {
	s := &PricingStore{cachePath: cachePath}
	if cachePath == "" {
		return s
	}
	f, err := os.Open(cachePath)
	if err != nil {
		return s
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxPricingBytes+1))
	if err != nil || len(data) > MaxPricingBytes {
		return s
	}
	var cache pricingCache
	if json.Unmarshal(data, &cache) != nil || cache.Source != "CloudPrice" {
		return s
	}
	stamp, err := time.Parse(time.RFC3339Nano, cache.UpdatedAt)
	if err != nil || stamp.Location() != time.UTC {
		return s
	}
	// Revalidate normalized disk data with the same required-field checks as an update.
	var envelope struct {
		Models json.RawMessage `json:"models"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return s
	}
	prices, err := parsePrices(envelope.Models)
	if err != nil || len(prices) != len(cache.Models) {
		return s
	}
	if cache.UpstreamUpdatedAt != "" {
		if _, err := time.Parse("2006-01-02", cache.UpstreamUpdatedAt); err != nil {
			return s
		}
	}
	cache.Models = prices
	s.cache = cache
	s.lower = buildLowerIndex(prices)
	return s
}

func (s *PricingStore) Status() PricingStatus {
	status := PricingStatus{Source: "CloudPrice", Status: "empty"}
	if s == nil {
		return status
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	status.ModelCount = len(s.cache.Models)
	if status.ModelCount > 0 {
		status.Status = "ready"
		status.UpdatedAt = s.cache.UpdatedAt
		status.UpstreamUpdatedAt = s.cache.UpstreamUpdatedAt
	}
	return status
}

// Update accepts a LiteLLM-compatible CloudPrice map. Non-token products and
// entries without both required token rates cannot price usage and are omitted.
// Every present token rate is still validated, including on omitted entries.
func (s *PricingStore) Update(data []byte) error {
	prices, err := parsePrices(data)
	if err != nil {
		return err
	}
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	cache := pricingCache{Source: "CloudPrice", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), Models: prices}
	var metadata struct {
		CloudPrice struct {
			UpdatedAt string `json:"updated_at"`
		} `json:"_cloudprice"`
	}
	if json.Unmarshal(data, &metadata) == nil {
		if _, err := time.Parse("2006-01-02", metadata.CloudPrice.UpdatedAt); err == nil {
			cache.UpstreamUpdatedAt = metadata.CloudPrice.UpdatedAt
		}
	}
	if s.cachePath != "" {
		data, err := json.Marshal(cache)
		if err != nil || len(data) > MaxPricingBytes {
			return errors.New("pricing cache exceeds limit")
		}
		if err := writePricingCache(s.cachePath, data); err != nil {
			return errors.New("pricing cache could not be saved")
		}
	}
	s.mu.Lock()
	s.cache = cache
	s.lower = buildLowerIndex(prices)
	s.mu.Unlock()
	return nil
}

func parsePrices(data []byte) (map[string]modelPrice, error) {
	if len(data) > MaxPricingBytes {
		return nil, errors.New("pricing data exceeds limit")
	}
	var entries map[string]json.RawMessage
	if json.Unmarshal(data, &entries) != nil || len(entries) == 0 || len(entries) > maxPricingModels {
		return nil, errors.New("invalid pricing map")
	}
	prices := make(map[string]modelPrice)
	for name, raw := range entries {
		if len(name) == 0 || len(name) > maxPricingModelName || strings.TrimSpace(name) != name || strings.ContainsAny(name, "\x00\r\n") {
			return nil, errors.New("invalid pricing model name")
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return nil, errors.New("invalid pricing model")
		}
		if name == "sample_spec" || name == "_cloudprice" {
			continue
		}
		rates := make([]*float64, 4)
		for i, key := range []string{"input_cost_per_token", "output_cost_per_token", "cache_read_input_token_cost", "cache_creation_input_token_cost"} {
			value, exists := fields[key]
			if !exists {
				continue
			}
			var rate float64
			if string(value) == "null" || json.Unmarshal(value, &rate) != nil || math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
				return nil, errors.New("invalid pricing rate")
			}
			rates[i] = &rate
		}
		if rates[0] == nil || rates[1] == nil {
			continue
		}
		prices[name] = modelPrice{Input: *rates[0], Output: *rates[1], CacheRead: rates[2], CacheWrite: rates[3]}
	}
	if len(prices) == 0 {
		return nil, errors.New("pricing map has no complete token prices")
	}
	return prices, nil
}

func buildLowerIndex(prices map[string]modelPrice) map[string]string {
	if len(prices) == 0 {
		return nil
	}
	m := make(map[string]string, len(prices))
	for k := range prices {
		m[strings.ToLower(k)] = k
	}
	return m
}

var defaultModelAliases = map[string]string{
	"deepseek-4.1-flash":          "deepseek-v4.1-flash",
	"deepseek/deepseek-4.1-flash": "deepseek/deepseek-v4.1-flash",
}

var reasoningSuffixes = []string{
	"-high",
	"-medium",
	"-low",
	"-minimal",
	"-none",
	"-max",
	"-thinking",
	"-thought",
}

var commonProviderPrefixes = []string{
	"gemini/",
	"google/",
	"openai/",
	"anthropic/",
	"deepseek/",
}

func writePricingCache(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".cloudprice-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}

func lookupModelPrice(prices map[string]modelPrice, lower map[string]string, name string) (modelPrice, bool) {
	if len(prices) == 0 {
		return modelPrice{}, false
	}
	for _, prefix := range []string{"[pi] ", "[omp] "} {
		name = strings.TrimPrefix(name, prefix)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return modelPrice{}, false
	}

	match := func(k string) (modelPrice, bool) {
		if p, ok := prices[k]; ok {
			return p, true
		}
		if target, exists := defaultModelAliases[k]; exists {
			if p, ok := prices[target]; ok {
				return p, true
			}
		}
		return modelPrice{}, false
	}

	// 1. Exact match and alias on full name
	if p, ok := match(name); ok {
		return p, true
	}

	// 2. Suffix stripping on full name (e.g. gemini-3.8-flash-high -> gemini-3.8-flash)
	for _, suffix := range reasoningSuffixes {
		if strings.HasSuffix(name, suffix) {
			base := strings.TrimSuffix(name, suffix)
			if p, ok := match(base); ok {
				return p, true
			}
		}
	}

	// 3. If name has no slash, try common CloudPrice provider prefixes
	if !strings.Contains(name, "/") {
		for _, provider := range commonProviderPrefixes {
			if p, ok := match(provider + name); ok {
				return p, true
			}
			for _, suffix := range reasoningSuffixes {
				if strings.HasSuffix(name, suffix) {
					if p, ok := match(provider + strings.TrimSuffix(name, suffix)); ok {
						return p, true
					}
				}
			}
		}
	}

	// 4. Case-insensitive fallback: normalize case first
	nameLower := strings.ToLower(name)
	matchLower := func(kLower string) (modelPrice, bool) {
		if lower != nil {
			if actual, ok := lower[kLower]; ok {
				return prices[actual], true
			}
		} else {
			for orig := range prices {
				if strings.ToLower(orig) == kLower {
					return prices[orig], true
				}
			}
		}
		if target, exists := defaultModelAliases[kLower]; exists {
			targetLower := strings.ToLower(target)
			if lower != nil {
				if actual, ok := lower[targetLower]; ok {
					return prices[actual], true
				}
			} else {
				for orig := range prices {
					if strings.ToLower(orig) == targetLower {
						return prices[orig], true
					}
				}
			}
		}
		return modelPrice{}, false
	}

	if p, ok := matchLower(nameLower); ok {
		return p, true
	}
	for _, suffix := range reasoningSuffixes {
		if strings.HasSuffix(nameLower, suffix) {
			if p, ok := matchLower(strings.TrimSuffix(nameLower, suffix)); ok {
				return p, true
			}
		}
	}
	if !strings.Contains(nameLower, "/") {
		for _, provider := range commonProviderPrefixes {
			if p, ok := matchLower(provider + nameLower); ok {
				return p, true
			}
			for _, suffix := range reasoningSuffixes {
				if strings.HasSuffix(nameLower, suffix) {
					if p, ok := matchLower(provider + strings.TrimSuffix(nameLower, suffix)); ok {
						return p, true
					}
				}
			}
		}
	}
	return modelPrice{}, false
}
func estimateModelCost(prices map[string]modelPrice, lower map[string]string, model ModelUsage) (float64, bool) {
	price, ok := lookupModelPrice(prices, lower, model.Model)
	if !ok || (model.CacheReadTokens > 0 && price.CacheRead == nil) || (model.CacheCreationTokens > 0 && price.CacheWrite == nil) {
		return 0, false
	}
	cost := float64(model.InputTokens)*price.Input + float64(model.OutputTokens)*price.Output
	if model.CacheReadTokens > 0 {
		cost += float64(model.CacheReadTokens) * *price.CacheRead
	}
	if model.CacheCreationTokens > 0 {
		cost += float64(model.CacheCreationTokens) * *price.CacheWrite
	}
	// Reasoning is already part of the adapter's output and is never charged again.
	return cost, !math.IsInf(cost, 0) && !math.IsNaN(cost)
}

func (s *PricingStore) Apply(rows []DailyUsage, source string) {
	var prices map[string]modelPrice
	var lower map[string]string
	if s != nil {
		s.mu.RLock()
		prices = s.cache.Models
		lower = s.lower
		s.mu.RUnlock()
	}
	for i := range rows {
		row := &rows[i]
		codex := source == "codex"
		originalMissing := row.MissingPricing
		if codex || row.CostEstimated {
			row.CostUSD = 0
			row.CostEstimated = false
			row.MissingPricing = false
		}
		var total uint64
		for _, model := range row.Models {
			total += model.TotalTokens
		}
		complete := total == row.TotalTokens
		sum, repaired := 0.0, 0.0
		allPriced := true
		for j := range row.Models {
			model := &row.Models[j]
			oldCost := model.CostUSD
			if model.CostEstimated {
				oldCost = nil
			}
			wasMissing := model.MissingPricing
			if !codex && oldCost != nil && *oldCost > 0 && !wasMissing {
				sum += *oldCost
				continue
			}
			if !codex && len(row.Models) == 1 && complete && row.CostUSD > 0 && !originalMissing && !wasMissing {
				value := row.CostUSD
				model.CostUSD = &value
				model.CostEstimated = false
				sum += value
				continue
			}
			value, ok := estimateModelCost(prices, lower, *model)
			if model.TotalTokens == 0 {
				value, ok = 0, true
			}
			if !ok {
				model.CostUSD = nil
				model.CostEstimated = false
				model.MissingPricing = true
				allPriced = false
				if codex || wasMissing || oldCost != nil || row.CostUSD == 0 {
					row.MissingPricing = true
				}
				continue
			}
			model.CostUSD = &value
			model.MissingPricing = false
			model.CostEstimated = model.TotalTokens > 0
			sum += value
			if wasMissing || (oldCost != nil && *oldCost == 0) {
				repaired += value
			}
		}
		if complete && len(row.Models) > 0 && allPriced {
			row.MissingPricing = false
		}
		if codex || row.CostUSD == 0 {
			row.CostUSD = sum
			for _, model := range row.Models {
				row.CostEstimated = row.CostEstimated || model.CostEstimated
			}
			if !complete || (row.TotalTokens > 0 && len(row.Models) == 0) {
				row.MissingPricing = true
			}
		} else if repaired > 0 && complete && sum > row.CostUSD {
			row.CostUSD = sum
			row.CostEstimated = true
		}
	}
}
