// Package channelpreset holds the built-in model-vendor preset
// library (backlog F35 P1): picking a vendor pre-fills a channel's
// base URL, models, trust type and max data level, so the operator
// only types the API key. The same list feeds the onboarding
// wizard (step 3) and the channels page — one source of truth, so a
// new vendor is added in one place.
//
// The entries are public endpoint facts (base URLs and model names
// from each vendor's public docs), not copied code; the list is
// data, not behaviour. A preset is only a starting point: every
// field stays editable in the form, and the trust type / max level
// it ships are the conservative defaults (signed-cloud vendors
// cap at internal, a local model may carry secret — see F17).
package channelpreset

import (
	"strings"

	"github.com/myjnathan/agentgate/internal/store"
)

// Preset is one vendor's channel defaults.
type Preset struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	NameEN       string   `json:"name_en"`
	BaseURL      string   `json:"base_url"`
	Models       []string `json:"models"`
	TrustType    string   `json:"trust_type"`
	MaxDataLevel string   `json:"max_data_level"`
	Note         string   `json:"note,omitempty"`
	NoteEN       string   `json:"note_en,omitempty"`
}

func preset(id, name, nameEN, base string, models []string, trust, level, note, noteEN string) Preset {
	return Preset{ID: id, Name: name, NameEN: nameEN, BaseURL: base,
		Models: models, TrustType: trust, MaxDataLevel: level, Note: note, NoteEN: noteEN}
}

// Presets returns the built-in library in display order. Custom
// stays last: it is the escape hatch for vendors not listed here.
func Presets() []Preset {
	return []Preset{
		preset("deepseek", "DeepSeek", "DeepSeek",
			"https://api.deepseek.com/v1",
			[]string{"deepseek-chat", "deepseek-reasoner"},
			"signed_cloud", "internal", "", ""),
		preset("qwen", "通义千问", "Qwen",
			"https://dashscope.aliyuncs.com/compatible-mode/v1",
			[]string{"qwen-plus", "qwen-turbo"},
			"signed_cloud", "internal", "", ""),
		preset("zhipu", "智谱 GLM", "Zhipu GLM",
			"https://open.bigmodel.cn/api/paas/v4",
			[]string{"glm-4-plus", "glm-4-flash"},
			"signed_cloud", "internal", "", ""),
		preset("openai", "OpenAI", "OpenAI",
			"https://api.openai.com/v1",
			[]string{"gpt-4o", "gpt-4o-mini"},
			"signed_cloud", "internal", "", ""),
		preset("anthropic", "Anthropic Claude", "Anthropic Claude",
			"https://api.anthropic.com/v1",
			[]string{"claude-sonnet-4-5", "claude-haiku-4-5"},
			"signed_cloud", "internal", "", ""),
		preset("moonshot", "Moonshot Kimi", "Moonshot Kimi",
			"https://api.moonshot.cn/v1",
			[]string{"kimi-k2", "moonshot-v1-8k"},
			"signed_cloud", "internal", "", ""),
		preset("doubao", "火山方舟", "Volcengine Ark",
			"https://ark.cn-beijing.volces.com/api/v3",
			[]string{"doubao-seed-1-6"},
			"signed_cloud", "internal",
			"模型填火山方舟的推理接入点 ID（不是模型原名）。",
			"Use the Ark inference endpoint ID as the model, not the base model name."),
		preset("ollama", "Ollama 本地模型", "Ollama (local)",
			"http://localhost:11434/v1",
			[]string{"llama3.1", "qwen2.5"},
			"local_model", "secret",
			"本地模型不经过任何云厂商，是唯一可承载绝密的渠道类型。",
			"A local model never leaves the machine — the only channel type that may carry top-secret data."),
		preset("custom", "自定义", "Custom", "", nil,
			"signed_cloud", "internal", "", ""),
	}
}

// ByID returns the preset with the given id, if any.
func ByID(id string) (Preset, bool) {
	for _, p := range Presets() {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// Valid reports whether p is a well-formed preset: a known trust
// type and data level, at least one model unless it is the custom
// escape hatch, and an https base URL (http only for the loopback
// local-model preset, which never leaves the machine).
func Valid(p Preset) bool {
	if p.ID == "" || p.Name == "" {
		return false
	}
	if _, ok := store.ParseTrustType(p.TrustType); !ok {
		return false
	}
	if _, ok := store.ParseDataLevel(p.MaxDataLevel); !ok {
		return false
	}
	if p.ID == "custom" {
		return p.BaseURL == "" && len(p.Models) == 0
	}
	if len(p.Models) == 0 || p.BaseURL == "" {
		return false
	}
	if strings.HasPrefix(p.BaseURL, "https://") {
		return true
	}
	return strings.HasPrefix(p.BaseURL, "http://localhost") ||
		strings.HasPrefix(p.BaseURL, "http://127.0.0.1")
}
