package config

import (
	"fmt"
	"sort"
	"strings"

	"CodeAgent/internal/llm"
)

// ProviderConfig 是单个 Provider 的连接与协议配置。
type ProviderConfig struct {
	BaseURL string                 `toml:"base_url"`
	APIKey  string                 `toml:"api_key"`
	API     string                 `toml:"api"`
	Headers map[string]string      `toml:"headers"`
	Models  map[string]ModelConfig `toml:"models"`
}

// ModelConfig 是单个模型的静态元数据与协议配置。
type ModelConfig struct {
	ID            string                `toml:"id"`
	DisplayName   string                `toml:"display_name"`
	API           string                `toml:"api"`
	Reasoning     bool                  `toml:"reasoning"`
	ContextWindow int                   `toml:"context_window"`
	MaxTokens     int                   `toml:"max_tokens"`
	Pricing       PricingConfig         `toml:"pricing"`
	OpenAI        *llm.OpenAIOptions    `toml:"openai"`
	Anthropic     *llm.AnthropicOptions `toml:"anthropic"`
}

// PricingConfig 是模型价格配置（单位：每百万 Token 价格）。
type PricingConfig struct {
	Input      float64 `toml:"input"`
	Output     float64 `toml:"output"`
	CacheRead  float64 `toml:"cache_read"`
	CacheWrite float64 `toml:"cache_write"`
}

// ModelEntry 是装配完成的模型及其默认请求选项。
type ModelEntry struct {
	Ref     string
	Model   llm.Model
	Options llm.RequestOptions
}

// validateModels 检查模型必要字段及协议，不修改配置。
func (c *Config) validateModels() error {
	for providerName, provider := range c.Providers {
		if strings.TrimSpace(provider.BaseURL) == "" {
			return fmt.Errorf("provider %q 的 base_url 不能为空", providerName)
		}

		if provider.API != "" && !isValidAPI(provider.API) {
			return fmt.Errorf("provider %q 的 api 协议 %q 无效，仅支持 %q 或 %q", providerName, provider.API, llm.APIKindOpenAI, llm.APIKindAnthropic)
		}

		for modelAlias, model := range provider.Models {
			ref := providerName + "/" + modelAlias

			if strings.TrimSpace(model.ID) == "" {
				return fmt.Errorf("模型 %s 的 id 不能为空", ref)
			}

			api := effectiveAPI(provider.API, model.API)
			if api == "" {
				return fmt.Errorf("模型 %s 未指定 api 协议，且 provider %q 亦未设置默认 api 协议", ref, providerName)
			}
			if !isValidAPI(api) {
				return fmt.Errorf("模型 %s 的 api 协议 %q 无效，仅支持 %q 或 %q", ref, api, llm.APIKindOpenAI, llm.APIKindAnthropic)
			}

			if model.ContextWindow < 0 {
				return fmt.Errorf("模型 %s 的 context_window 不能为负数", ref)
			}
			if model.MaxTokens < 0 {
				return fmt.Errorf("模型 %s 的 max_tokens 不能为负数", ref)
			}
			if model.Pricing.Input < 0 || model.Pricing.Output < 0 || model.Pricing.CacheRead < 0 || model.Pricing.CacheWrite < 0 {
				return fmt.Errorf("模型 %s 的 pricing 参数不能为负数", ref)
			}
		}
	}
	return nil
}

// ModelEntries 遍历已加载配置，一次性装配并返回可用模型列表（按 Ref 排序）。
// 返回项的 Headers、OpenAI 和 Anthropic 与配置共享 map 或指针，调用方和适配器必须保持只读。
// 单次请求需要修改这些数据时，先复制对应的 map 或选项结构体，再将副本赋给该请求；仅复制 RequestOptions 不会复制其共享数据。
func (c *Config) ModelEntries() []ModelEntry {
	var entries []ModelEntry

	for providerName, provider := range c.Providers {
		for modelAlias, modelCfg := range provider.Models {
			ref := providerName + "/" + modelAlias

			api := effectiveAPI(provider.API, modelCfg.API)

			displayName := modelCfg.DisplayName
			if displayName == "" {
				displayName = modelAlias
			}

			pricing := llm.Pricing{
				Input:      modelCfg.Pricing.Input,
				Output:     modelCfg.Pricing.Output,
				CacheRead:  modelCfg.Pricing.CacheRead,
				CacheWrite: modelCfg.Pricing.CacheWrite,
			}

			model := llm.Model{
				Provider:      providerName,
				ID:            modelCfg.ID,
				DisplayName:   displayName,
				API:           llm.APIKind(api),
				BaseURL:       provider.BaseURL,
				Reasoning:     modelCfg.Reasoning,
				ContextWindow: modelCfg.ContextWindow,
				MaxTokens:     modelCfg.MaxTokens,
				Pricing:       pricing,
			}

			options := llm.RequestOptions{
				APIKey:    provider.APIKey,
				Headers:   provider.Headers,
				OpenAI:    modelCfg.OpenAI,
				Anthropic: modelCfg.Anthropic,
			}

			entries = append(entries, ModelEntry{
				Ref:     ref,
				Model:   model,
				Options: options,
			})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Ref < entries[j].Ref
	})

	return entries
}

// effectiveAPI 优先使用模型协议，未指定时继承 Provider 协议。
func effectiveAPI(providerAPI, modelAPI string) string {
	if modelAPI != "" {
		return modelAPI
	}
	return providerAPI
}

func isValidAPI(api string) bool {
	return api == string(llm.APIKindOpenAI) || api == string(llm.APIKindAnthropic)
}
