package rerank

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type deadlineCapturingReranker struct {
	deadline    time.Time
	hasDeadline bool
}

func (r *deadlineCapturingReranker) Rerank(ctx context.Context, _ string, _ []string) ([]RankResult, error) {
	r.deadline, r.hasDeadline = ctx.Deadline()
	return nil, nil
}

func (r *deadlineCapturingReranker) GetModelName() string { return "test-reranker" }
func (r *deadlineCapturingReranker) GetModelID() string   { return "test-model" }

func TestConfiguredRerankTimeout(t *testing.T) {
	t.Run("uses default for an unset value", func(t *testing.T) {
		t.Setenv(rerankTimeoutEnv, "")
		if got := configuredRerankTimeout(); got != defaultRerankTimeout {
			t.Fatalf("timeout = %v, want %v", got, defaultRerankTimeout)
		}
	})
	t.Run("uses positive seconds from the environment", func(t *testing.T) {
		t.Setenv(rerankTimeoutEnv, "42")
		if got := configuredRerankTimeout(); got != 42*time.Second {
			t.Fatalf("timeout = %v, want 42s", got)
		}
	})
	t.Run("falls back for an invalid value", func(t *testing.T) {
		t.Setenv(rerankTimeoutEnv, "not-a-number")
		if got := configuredRerankTimeout(); got != defaultRerankTimeout {
			t.Fatalf("timeout = %v, want %v", got, defaultRerankTimeout)
		}
	})
}

func TestFallbackTimeoutRerankerRespectsCallerDeadline(t *testing.T) {
	inner := &deadlineCapturingReranker{}
	reranker := &fallbackTimeoutReranker{inner: inner, timeout: 10 * time.Millisecond}
	parent, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := reranker.Rerank(parent, "query", []string{"document"}); err != nil {
		t.Fatal(err)
	}
	if !inner.hasDeadline {
		t.Fatal("reranker did not receive the caller deadline")
	}
	if remaining := time.Until(inner.deadline); remaining < time.Second {
		t.Fatalf("caller deadline was truncated by fallback timeout: remaining=%v", remaining)
	}
}

func TestNewRerankerAppliesFallbackTimeoutWithoutCallerDeadline(t *testing.T) {
	withRerankSSRFWhitelist(t, "127.0.0.1")
	t.Setenv(rerankTimeoutEnv, "1")

	requestStarted := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestStarted <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	reranker, err := NewReranker(&RerankerConfig{
		Provider:  "zhipu",
		BaseURL:   server.URL,
		ModelName: "test-reranker",
	})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = reranker.Rerank(context.Background(), "query", []string{"document"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("rerank error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("fallback timeout returned too late: %s", elapsed)
	}
	select {
	case <-requestStarted:
	default:
		t.Fatal("fallback timeout expired before the local rerank server received the request")
	}
}
