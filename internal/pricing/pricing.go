// Package pricing holds the per-model pricing table and usage-budget limits
// for OpenCode Go subscriptions. Prices are in USD per 1,000,000 tokens.
//
// Source: https://opencode.ai/docs/zh-cn/go/ (retrieved Jul 2026).
//
// Each subscription is subject to three rolling budgets:
//   - 5-hour window:  $12 USD
//   - weekly window:  $30 USD
//   - monthly window: $60 USD (or $15 for premium-tier models)
//
// The monthly limit varies by model because some models are subsidised more
// heavily than others. We store the most restrictive limit the model can
// be used against.
package pricing

// ModelPricing describes the unit price for a single model.
// All fields are USD per 1M tokens.
type ModelPricing struct {
	Input        float64
	Output       float64
	CacheRead    float64
	MonthlyLimit float64 // USD budget this model draws from ($15 or $60)
}

// FiveHourLimit is the USD budget per 5-hour rolling window for every key.
const FiveHourLimit = 12.0

// WeeklyLimit is the USD budget per rolling week for every key.
const WeeklyLimit = 30.0

// Models is the authoritative price table keyed by model ID.
var Models = map[string]ModelPricing{
	"grok-4.5":          {Input: 2.00, Output: 6.00, CacheRead: 0.30, MonthlyLimit: 15},
	"glm-5.2":           {Input: 1.40, Output: 4.40, CacheRead: 0.26, MonthlyLimit: 60},
	"glm-5.1":           {Input: 1.40, Output: 4.40, CacheRead: 0.26, MonthlyLimit: 60},
	"kimi-k3":           {Input: 3.00, Output: 15.00, CacheRead: 0.30, MonthlyLimit: 15},
	"kimi-k2.7-code":    {Input: 0.95, Output: 4.00, CacheRead: 0.19, MonthlyLimit: 60},
	"kimi-k2.6":         {Input: 0.95, Output: 4.00, CacheRead: 0.16, MonthlyLimit: 60},
	"deepseek-v4-pro":   {Input: 0.435, Output: 0.87, CacheRead: 0.003625, MonthlyLimit: 15},
	"deepseek-v4-flash": {Input: 0.14, Output: 0.28, CacheRead: 0.0028, MonthlyLimit: 60},
	"mimo-v2.5":         {Input: 0.14, Output: 0.28, CacheRead: 0.0028, MonthlyLimit: 60},
	"mimo-v2.5-pro":     {Input: 0.435, Output: 0.87, CacheRead: 0.003625, MonthlyLimit: 15},
	"minimax-m3":        {Input: 0.30, Output: 1.20, CacheRead: 0.06, MonthlyLimit: 60},
	"minimax-m2.7":      {Input: 0.30, Output: 1.20, CacheRead: 0.06, MonthlyLimit: 60},
	"minimax-m2.5":      {Input: 0.30, Output: 1.20, CacheRead: 0.06, MonthlyLimit: 60},
	"qwen3.7-max":       {Input: 2.50, Output: 7.50, CacheRead: 0.50, MonthlyLimit: 60},
	"qwen3.7-plus":      {Input: 0.40, Output: 1.60, CacheRead: 0.04, MonthlyLimit: 60},
	"qwen3.6-plus":      {Input: 0.50, Output: 3.00, CacheRead: 0.05, MonthlyLimit: 60},
	"hy3":               {Input: 0.14, Output: 0.58, CacheRead: 0.035, MonthlyLimit: 60},
}

// Models is the authoritative price table keyed by model ID.
// If the model is unknown, returns 0 (the request is still logged but does
// not count against any budget).
//
// Parameters:
//   - model: the bare model ID (e.g. "kimi-k3")
//   - promptTokens: total input tokens (including cached)
//   - completionTokens: output tokens
//   - cachedTokens: the subset of prompt tokens that were cache hits
func Cost(model string, promptTokens, completionTokens, cachedTokens int64) float64 {
	p, ok := Models[model]
	if !ok {
		return 0
	}
	// Cached tokens are billed at the cache_read rate instead of the full
	// input rate. Non-cached input = prompt - cached.
	nonCached := promptTokens - cachedTokens
	if nonCached < 0 {
		nonCached = 0
	}
	return float64(nonCached)*p.Input/1e6 +
		float64(cachedTokens)*p.CacheRead/1e6 +
		float64(completionTokens)*p.Output/1e6
}
