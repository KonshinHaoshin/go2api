package proxy

import "strings"

// UpstreamFamily identifies which upstream protocol a model uses.
type UpstreamFamily string

const (
	// FamilyOpenAI: model is hosted on the OpenAI-compatible endpoint
	// (https://opencode.ai/zen/go/v1/chat/completions).
	FamilyOpenAI UpstreamFamily = "openai"
	// FamilyAnthropic: model is hosted on the Anthropic-compatible endpoint
	// (https://opencode.ai/zen/go/v1/messages).
	FamilyAnthropic UpstreamFamily = "anthropic"
)

// DefaultModelFamilies maps the OpenCode Go model IDs to their upstream
// family. Anything not listed defaults to FamilyOpenAI so we still send the
// request somewhere reasonable; the upstream will then return its own "not
// supported" error if the model truly isn't available.
//
// Source: https://opencode.ai/docs/zh-cn/go/
//
//   - chat/completions (OpenAI-compatible): Grok, GLM, Kimi, DeepSeek, MiMo, Hy3
//   - messages (Anthropic-compatible):       MiniMax, Qwen
var DefaultModelFamilies = map[string]UpstreamFamily{
	// OpenAI-compatible
	"grok-4.5":          FamilyOpenAI,
	"glm-5.2":           FamilyOpenAI,
	"glm-5.1":           FamilyOpenAI,
	"kimi-k3":           FamilyOpenAI,
	"kimi-k2.7-code":    FamilyOpenAI,
	"kimi-k2.6":         FamilyOpenAI,
	"deepseek-v4-pro":   FamilyOpenAI,
	"deepseek-v4-flash": FamilyOpenAI,
	"mimo-v2.5":         FamilyOpenAI,
	"mimo-v2.5-pro":     FamilyOpenAI,
	"hy3":               FamilyOpenAI,

	// Anthropic-compatible
	"minimax-m3":   FamilyAnthropic,
	"minimax-m2.7": FamilyAnthropic,
	"minimax-m2.5": FamilyAnthropic,
	"qwen3.7-max":  FamilyAnthropic,
	"qwen3.7-plus": FamilyAnthropic,
	"qwen3.6-plus": FamilyAnthropic,
}

// ModelFamily returns the upstream family for the given model ID.
// The model ID may be passed with or without the "opencode-go/" prefix.
func ModelFamily(model string) UpstreamFamily {
	id := strings.TrimPrefix(model, "opencode-go/")
	if f, ok := DefaultModelFamilies[id]; ok {
		return f
	}
	return FamilyOpenAI
}

// EndpointForFamily returns the upstream path that this family uses.
func EndpointForFamily(f UpstreamFamily) string {
	switch f {
	case FamilyAnthropic:
		return "/messages"
	default:
		return "/chat/completions"
	}
}
