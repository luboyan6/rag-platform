package rerank

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/provider"
	"github.com/Tencent/WeKnora/internal/types"
)

// Reranker defines the interface for document reranking
type Reranker interface {
	// Rerank reranks documents based on relevance to the query
	Rerank(ctx context.Context, query string, documents []string) ([]RankResult, error)

	// GetModelName returns the model name
	GetModelName() string

	// GetModelID returns the model ID
	GetModelID() string
}

const (
	rerankTimeoutEnv     = "WEKNORA_RERANK_TIMEOUT_SECONDS"
	defaultRerankTimeout = 30 * time.Second
)

// configuredRerankTimeout returns the fallback timeout for a rerank request.
// It is used only when the caller has not already supplied a context deadline.
// The value is expressed in seconds to match the other model timeout settings.
func configuredRerankTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv(rerankTimeoutEnv))
	if raw == "" {
		return defaultRerankTimeout
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seconds <= 0 || seconds > int64((time.Duration(1<<63-1))/time.Second) {
		return defaultRerankTimeout
	}
	return time.Duration(seconds) * time.Second
}

// withRerankTimeout adds a fallback deadline only when the caller has not
// already chosen one. A caller-provided deadline, including a longer one,
// remains the authoritative timeout policy.
func withRerankTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// fallbackTimeoutReranker applies the factory-wide fallback timeout to every
// provider implementation without duplicating deadline handling in each one.
type fallbackTimeoutReranker struct {
	inner   Reranker
	timeout time.Duration
}

func (r *fallbackTimeoutReranker) Rerank(ctx context.Context, query string, documents []string) ([]RankResult, error) {
	timeoutCtx, cancel := withRerankTimeout(ctx, r.timeout)
	defer cancel()
	return r.inner.Rerank(timeoutCtx, query, documents)
}

func (r *fallbackTimeoutReranker) GetModelName() string { return r.inner.GetModelName() }
func (r *fallbackTimeoutReranker) GetModelID() string   { return r.inner.GetModelID() }

type RankResult struct {
	Index          int          `json:"index"`
	Document       DocumentInfo `json:"document"`
	RelevanceScore float64      `json:"relevance_score"`
}

// Handles the RelevanceScore field by checking if RelevanceScore exists first, otherwise falls back to Score field
func (r *RankResult) UnmarshalJSON(data []byte) error {
	var temp struct {
		Index          int          `json:"index"`
		Document       DocumentInfo `json:"document"`
		RelevanceScore *float64     `json:"relevance_score"`
		Score          *float64     `json:"score"`
	}

	if err := json.Unmarshal(data, &temp); err != nil {
		return fmt.Errorf("failed to unmarshal rank result: %w", err)
	}

	r.Index = temp.Index
	r.Document = temp.Document

	if temp.RelevanceScore != nil {
		r.RelevanceScore = *temp.RelevanceScore
	} else if temp.Score != nil {
		r.RelevanceScore = *temp.Score
	}

	return nil
}

type DocumentInfo struct {
	Text string `json:"text"`
}

// UnmarshalJSON handles both string and object formats for DocumentInfo
func (d *DocumentInfo) UnmarshalJSON(data []byte) error {
	// First try to unmarshal as a string
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		d.Text = text
		return nil
	}

	// If that fails, try to unmarshal as an object with text field
	var temp struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &temp); err != nil {
		return fmt.Errorf("failed to unmarshal DocumentInfo: %w", err)
	}

	d.Text = temp.Text
	return nil
}

type RerankerConfig struct {
	APIKey      string
	BaseURL     string
	ModelName   string
	Source      types.ModelSource
	ModelID     string
	Provider    string // Provider identifier: openai, aliyun, zhipu, siliconflow, jina, generic
	ExtraConfig map[string]string
	// CustomHeaders 允许在调用远程 API 时附加自定义 HTTP 请求头（类似 OpenAI Python SDK 的 extra_headers）。
	CustomHeaders map[string]string
	AppID         string
	AppSecret     string // 加密值，工厂函数调用方传入，使用前已解密
}

// ConfigFromModel 根据 types.Model 构造 RerankerConfig。
// 生产路径（从 DB 拉起）和测试连接路径（临时表单）共享这份映射。
// appID / appSecret 是已解密的 WeKnoraCloud 凭证，调用方负责传入。
func ConfigFromModel(m *types.Model, appID, appSecret string) *RerankerConfig {
	if m == nil {
		return nil
	}
	return &RerankerConfig{
		ModelID:       m.ID,
		APIKey:        m.Parameters.APIKey,
		BaseURL:       m.Parameters.BaseURL,
		ModelName:     m.Name,
		Source:        m.Source,
		Provider:      m.Parameters.Provider,
		ExtraConfig:   m.Parameters.ExtraConfig,
		CustomHeaders: m.Parameters.CustomHeaders,
		AppID:         appID,
		AppSecret:     appSecret,
	}
}

// NewReranker creates a reranker based on the configuration
func NewReranker(config *RerankerConfig) (Reranker, error) {
	r, err := newReranker(config)
	if err != nil {
		return r, err
	}
	if logger.LLMDebugEnabled() {
		r = &debugReranker{inner: r}
	}
	r, err = wrapRerankerLangfuse(r, nil)
	if err != nil {
		return r, err
	}
	// Keep this outermost so debug and Langfuse wrappers observe the same
	// fallback deadline as the provider implementation.
	return &fallbackTimeoutReranker{inner: r, timeout: configuredRerankTimeout()}, nil
}

// customHeaderSetter 表示支持注入自定义 HTTP header 的 reranker 实现。
type customHeaderSetter interface {
	SetCustomHeaders(map[string]string)
}

func newReranker(config *RerankerConfig) (Reranker, error) {
	// Use provider field if set, otherwise detect from URL using provider registry
	providerName := provider.ProviderName(config.Provider)
	if providerName == "" {
		providerName = provider.DetectProvider(config.BaseURL)
	}

	var (
		reranker Reranker
		err      error
	)
	switch providerName {
	case provider.ProviderAliyun:
		reranker, err = NewAliyunReranker(config)
	case provider.ProviderZhipu:
		reranker, err = NewZhipuReranker(config)
	case provider.ProviderJina:
		reranker, err = NewJinaReranker(config)
	case provider.ProviderNvidia:
		reranker, err = NewNvidiaReranker(config)
	case provider.ProviderWeKnoraCloud:
		reranker, err = NewWeKnoraCloudReranker(config)
	case provider.ProviderLKEAP:
		reranker, err = NewLKEAPReranker(config)
	case provider.ProviderVolcengine:
		reranker, err = NewVolcengineReranker(config)
	default:
		reranker, err = NewOpenAIReranker(config)
	}
	if err != nil {
		return nil, err
	}
	if setter, ok := reranker.(customHeaderSetter); ok {
		setter.SetCustomHeaders(config.CustomHeaders)
	}
	return reranker, nil
}
