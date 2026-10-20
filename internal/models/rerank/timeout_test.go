package rerank

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/models/imageprep"
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

type deadlineCapturingImageReranker struct {
	deadlineCapturingReranker
	limits imageprep.Limits
	call   func(context.Context) ([]RankResult, error)
}

func (r *deadlineCapturingImageReranker) AcceptsImages() bool           { return true }
func (r *deadlineCapturingImageReranker) ImageLimits() imageprep.Limits { return r.limits }
func (r *deadlineCapturingImageReranker) RerankImages(ctx context.Context, _ string, _ []Image) ([]RankResult, error) {
	r.deadline, r.hasDeadline = ctx.Deadline()
	if r.call != nil {
		return r.call(ctx)
	}
	return nil, nil
}

func TestFallbackTimeoutRerankerDelegatesImageCapabilities(t *testing.T) {
	limits := imageprep.Limits{MaxBytes: 1024, MIMETypes: []string{"image/png"}}
	reranker := &fallbackTimeoutReranker{inner: &deadlineCapturingImageReranker{limits: limits}}
	ir, ok := AsImageReranker(reranker)
	if !ok {
		t.Fatal("wrapped image reranker lost image support")
	}
	if got := ir.ImageLimits(); !reflect.DeepEqual(got, limits) {
		t.Fatalf("image limits = %v, want %v", got, limits)
	}
}

func TestFallbackTimeoutRerankerImagesRespectsCallerDeadline(t *testing.T) {
	for _, timeout := range []time.Duration{time.Second, time.Minute} {
		t.Run(timeout.String(), func(t *testing.T) {
			inner := &deadlineCapturingImageReranker{}
			reranker := &fallbackTimeoutReranker{inner: inner, timeout: 10 * time.Second}
			parent, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			if _, err := reranker.RerankImages(parent, "query", nil); err != nil {
				t.Fatal(err)
			}
			want, _ := parent.Deadline()
			if !inner.hasDeadline || !inner.deadline.Equal(want) {
				t.Fatalf("image deadline = %v (set=%v), want caller deadline %v", inner.deadline, inner.hasDeadline, want)
			}
		})
	}
}

func TestFallbackTimeoutRerankerImagesCancelsWithoutCallerDeadline(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	inner := &deadlineCapturingImageReranker{call: func(ctx context.Context) ([]RankResult, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return nil, nil
		}
	}}
	reranker := &fallbackTimeoutReranker{inner: inner, timeout: 10 * time.Millisecond}
	done := make(chan error, 1)
	go func() {
		_, err := reranker.RerankImages(context.Background(), "query", nil)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("image rerank error = %v, want context deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("image rerank did not terminate at its fallback deadline")
	}
}

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
