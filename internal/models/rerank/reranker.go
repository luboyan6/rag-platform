// Package rerank adapts rerank protocols to application batching and score semantics.
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
	"github.com/Tencent/WeKnora/internal/models/api"
	"github.com/Tencent/WeKnora/internal/models/api/cohererank"
	"github.com/Tencent/WeKnora/internal/models/api/dashscoperank"
	"github.com/Tencent/WeKnora/internal/models/api/nimrerank"
	modelruntime "github.com/Tencent/WeKnora/internal/models/runtime"

	// modelruntime.Resolve answers from the vendor catalog, which is empty until
	// the vendor packages have run their init. Without this import every row
	// resolves to the generic vendor: LKEAP and Volcengine would lose their
	// signed clients and Aliyun its native protocol.
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

// PassageLimiter is implemented by rerankers whose vendor documents how large
// a document may be. The rerank stage builds its passages itself (title,
// chunk body, captions, OCR text, generated questions), so it can fit them to
// the limit instead of letting one oversized candidate fail the whole
// request, which the protocol layer rightly refuses to truncate on its own.
type PassageLimiter interface {
	// MaxPassageRunes returns the longest document, in runes, one request
	// can carry beside query. 0 means no documented limit.
	MaxPassageRunes(query string) int
}

// MaxPassageRunes reports r's passage limit for query, or 0 when r documents
// none.
func MaxPassageRunes(r Reranker, query string) int {
	if limiter, ok := r.(PassageLimiter); ok {
		return limiter.MaxPassageRunes(query)
	}
	return 0
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

// MaxPassageRunes forwards the wrapped reranker's limit.
func (r *fallbackTimeoutReranker) MaxPassageRunes(query string) int {
	return MaxPassageRunes(r.inner, query)
}
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
	Provider    string                   // Provider identifier: openai, aliyun, zhipu, siliconflow, jina, generic
	Spec        *types.ModelSpecOverride `json:"spec,omitempty"`
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
		Spec:          m.Parameters.Spec,
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

// newReranker resolves the catalog and returns the protocol client for the
// configured model, wrapped in the shared batching and score-scaling layer.
// It mirrors chat.NewRemoteChat: the vendor's facts decide the protocol, the
// URL and the credential, and this function knows no vendor names.
func newReranker(config *RerankerConfig) (Reranker, error) {
	if config == nil {
		return nil, fmt.Errorf("rerank config is nil")
	}
	resolved, err := modelruntime.Resolve(modelruntime.Ref{
		Provider:  config.Provider,
		Model:     config.ModelName,
		BaseURL:   config.BaseURL,
		ModelType: types.ModelTypeRerank,
		Extra:     config.ExtraConfig,
		Override:  config.Spec,
	})
	if err != nil {
		return nil, err
	}
	if err := validateRerankBaseURL(resolved.BaseURL); err != nil {
		return nil, err
	}

	vendor := resolved.Vendor
	endpoint, err := resolved.Endpoint(types.ModelTypeRerank, modelruntime.Connection{
		ModelID:     config.ModelID,
		Credentials: api.Credentials{APIKey: config.APIKey, AppID: config.AppID, AppSecret: config.AppSecret},
		Headers:     config.CustomHeaders,
		Extra:       config.ExtraConfig,
		Client:      newRerankHTTPClient(time.Duration(resolved.Rerank.RequestTimeout) * time.Second),
	})
	if err != nil {
		return nil, err
	}

	var client api.Reranker
	switch resolved.RerankAPI {
	case api.RerankCohere:
		client = cohererank.New(cohererank.Config{Endpoint: endpoint, Settings: resolved.Rerank})
	case api.RerankDashScope:
		client = dashscoperank.New(dashscoperank.Config{Endpoint: endpoint, Settings: resolved.Rerank})
	case api.RerankNIM:
		client = nimrerank.New(nimrerank.Config{Endpoint: endpoint, Settings: resolved.Rerank})
	case api.RerankTencentLKEAP:
		client, err = newLKEAPClient(config, resolved)
	case api.RerankVolcengineKnowledge:
		client, err = newVolcengineClient(config, resolved)
	default:
		return nil, fmt.Errorf("unsupported rerank api %q for provider %s", resolved.RerankAPI, vendor.ID)
	}
	if err != nil {
		return nil, err
	}

	return &protocolReranker{
		inner:     client,
		settings:  resolved.Rerank,
		endpoint:  resolved.BaseURL,
		modelName: config.ModelName,
		modelID:   config.ModelID,
	}, nil
}
