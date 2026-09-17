package langfuse

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
)

// dummyPayload is a minimal payload that embeds TracingContext, mirroring
// how real asynq payloads opt into trace propagation.
type dummyPayload struct {
	types.TracingContext
	KnowledgeID string `json:"knowledge_id"`
}

// TestInjectTracing_DisabledIsZero verifies InjectTracing is a no-op when
// Langfuse is disabled: no panics, no trace fields written.
func TestInjectTracing_DisabledIsZero(t *testing.T) {
	_, _ = Init(Config{Enabled: false})

	p := &dummyPayload{KnowledgeID: "k1"}
	InjectTracing(context.Background(), p)
	if p.LangfuseTraceparent != "" || p.LangfuseTraceID != "" {
		t.Fatalf("expected no tracing fields on disabled manager, got %+v", p.TracingContext)
	}
}

// TestInjectTracing_PopulatesTraceparent checks that when a trace is active
// on the context, a W3C traceparent is stamped onto the payload (so the
// asynq worker can resume the same trace).
func TestInjectTracing_PopulatesTraceparent(t *testing.T) {
	m, _ := newTestManager(t)

	ctx, trace := m.StartTrace(context.Background(), TraceOptions{Name: "parent"})
	p := &dummyPayload{KnowledgeID: "k1"}
	InjectTracing(ctx, p)

	if p.LangfuseTraceparent == "" {
		t.Fatal("expected LangfuseTraceparent to be populated")
	}
	// The traceparent carries the trace id; trace.ID is the OTel trace id.
	if !strings.HasPrefix(p.LangfuseTraceparent, "00-"+trace.ID) {
		t.Errorf("traceparent %q does not carry trace id %s", p.LangfuseTraceparent, trace.ID)
	}
	if p.LangfuseTraceID != trace.ID {
		t.Errorf("LangfuseTraceID = %q, want %q", p.LangfuseTraceID, trace.ID)
	}
}

// TestAsynqMiddleware_TraceparentPropagation is the cross-process correlation
// core test: InjectTracing stamps a traceparent onto the payload; the worker
// middleware re-extracts it, and the worker span inherits the upstream trace
// id — stitching the HTTP trace and the async job into one LiteFuse tree.
func TestAsynqMiddleware_TraceparentPropagation(t *testing.T) {
	m, exp := newTestManager(t)

	// Upstream caller opens a span and injects a traceparent onto the payload.
	upstreamCtx, upstreamTrace := m.StartTrace(context.Background(), TraceOptions{
		Name:      "POST /api/v1/knowledge-chat/:session_id",
		UserID:    "user-async",
		SessionID: "session-async",
		Tags:      []string{"http", "post"},
	})
	remoteTraceID := upstreamTrace.ID
	payload := &dummyPayload{KnowledgeID: "k1"}
	InjectTracing(upstreamCtx, payload)
	if payload.LangfuseTraceparent == "" {
		t.Fatal("InjectTracing did not stamp a traceparent")
	}
	raw, _ := json.Marshal(payload)

	mw := AsynqMiddleware()(asynq.HandlerFunc(func(context.Context, *asynq.Task) error { return nil }))
	if err := mw.ProcessTask(context.Background(), asynq.NewTask("test:type", raw)); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	upstreamTrace.Finish(nil, nil)

	for _, s := range exp.GetSpans() {
		if s.Name != "asynq.task" {
			continue
		}
		if s.SpanContext.TraceID().String() != remoteTraceID {
			t.Errorf("worker span trace id = %s, want upstream %s (traceparent not propagated)",
				s.SpanContext.TraceID(), remoteTraceID)
		}
		if spanType(s) != obsTypeChain ||
			spanAttr(s.Attributes, attrLangfuseUserID) != "user-async" ||
			spanAttr(s.Attributes, attrLangfuseSessionID) != "session-async" ||
			spanAttr(s.Attributes, attrTraceName) != "POST /api/v1/knowledge-chat/:session_id" ||
			len(spanStringSliceAttr(s.Attributes, attrTraceTags)) != 2 {
			t.Errorf("worker span lost trace-wide context: type=%q user=%q session=%q name=%q tags=%#v",
				spanType(s), spanAttr(s.Attributes, attrLangfuseUserID),
				spanAttr(s.Attributes, attrLangfuseSessionID), spanAttr(s.Attributes, attrTraceName),
				spanStringSliceAttr(s.Attributes, attrTraceTags))
		}
		return
	}
	t.Fatal("asynq worker span not exported")
}

// TestAsynqMiddleware_StandaloneTrace asserts that when the payload carries
// NO upstream traceparent (e.g. a scheduled job), the middleware opens a
// stable standalone root and keeps the task type in metadata/tags.
func TestAsynqMiddleware_StandaloneTrace(t *testing.T) {
	_, exp := newTestManager(t)

	payload := &dummyPayload{KnowledgeID: "kX"}
	raw, _ := json.Marshal(payload)

	mw := AsynqMiddleware()(asynq.HandlerFunc(func(context.Context, *asynq.Task) error { return nil }))
	if err := mw.ProcessTask(context.Background(), asynq.NewTask("scheduled:ping", raw)); err != nil {
		t.Fatalf("handler err: %v", err)
	}

	// The standalone run opens a root observation ("asynq.run", type=span)
	// plus a worker observation ("asynq.task", type=chain).
	var sawRoot, sawSpan bool
	for _, s := range exp.GetSpans() {
		if s.Name == "asynq.run" && !s.Parent.IsValid() {
			sawRoot = true
		}
		if s.Name == "asynq.task" && spanType(s) == obsTypeChain {
			sawSpan = true
		}
	}
	if !sawRoot {
		t.Error("standalone run should open a root observation named asynq.run")
	}
	if !sawSpan {
		t.Error("standalone run should open a worker span")
	}
}

func TestSpanInputFromPayloadRedactsSecrets(t *testing.T) {
	input := spanInputFromPayload([]byte(`{"query":"hello","api_key":"secret-value","nested":{"password":"password-value"},"lf_traceparent":"trace"}`))
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal redacted input: %v", err)
	}
	got := string(encoded)
	if !strings.Contains(got, "hello") {
		t.Fatalf("redacted payload lost useful query: %s", got)
	}
	for _, secret := range []string{"secret-value", "password-value", "trace"} {
		if strings.Contains(got, secret) {
			t.Fatalf("payload secret %q leaked into trace input: %s", secret, got)
		}
	}
}

func TestSanitizeTraceQueryRedactsCredentials(t *testing.T) {
	got := sanitizeTraceQuery("q=hello&access_token=secret&state=opaque&limit=5")
	if !strings.Contains(got, "q=hello") || !strings.Contains(got, "limit=5") {
		t.Fatalf("useful query dimensions lost: %s", got)
	}
	for _, secret := range []string{"secret", "opaque"} {
		if strings.Contains(got, secret) {
			t.Fatalf("query secret %q leaked: %s", secret, got)
		}
	}
}
