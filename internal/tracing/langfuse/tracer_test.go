package langfuse

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// newTestManager builds a Manager wired to an in-memory span exporter via the
// SimpleSpanProcessor (synchronous export on span End), so tests can assert on
// exported spans deterministically without an HTTP server.
func newTestManager(t *testing.T) (*Manager, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	m, err := Init(Config{
		Enabled:        true,
		Host:           "http://test",
		PublicKey:      "pk",
		SecretKey:      "sk",
		FlushAt:        1,
		FlushInterval:  1 * time.Second,
		QueueSize:      32,
		RequestTimeout: 2 * time.Second,
		SampleRate:     1.0,
		testExporter:   exp,
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	return m, exp
}

func TestStartChildSpanSkipsUntracedPolling(t *testing.T) {
	m, exp := newTestManager(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		for _, name := range []string{"sandbox.connect", "sandbox.exec"} {
			childCtx, child := m.StartChildSpan(ctx, SpanOptions{Name: name})
			child.Finish(nil, nil, nil)
			if childCtx != ctx || oteltrace.SpanContextFromContext(childCtx).IsValid() {
				t.Fatal("polling without a parent must not create a trace")
			}
		}
	}
	if spans := exp.GetSpans(); len(spans) != 0 {
		t.Fatalf("untraced polling exported %d spans", len(spans))
	}
}

func TestStartChildSpanPreservesTaskHierarchy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	// The Docker SDK uses this transport, which obtains its tracer provider
	// from the active context rather than the global provider.
	client := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport,
		otelhttp.WithSpanNameFormatter(func(_ string, _ *http.Request) string { return "docker.http" }))}
	for _, async := range []bool{false, true} {
		t.Run(map[bool]string{false: "agent", true: "async_install"}[async], func(t *testing.T) {
			m, exp := newTestManager(t)
			ctx, cancel := context.WithCancel(context.Background())
			ctx, task := m.StartSpan(ctx, SpanOptions{Name: "task"})
			if async {
				ctx = context.WithoutCancel(ctx)
				cancel()
			}
			for _, name := range []string{"sandbox.connect", "sandbox.exec", "sandbox.create_snapshot"} {
				childCtx, child := m.StartChildSpan(ctx, SpanOptions{Name: name})
				req, err := http.NewRequestWithContext(childCtx, http.MethodGet, server.URL, nil)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				child.Finish(nil, nil, nil)
			}
			task.Finish(nil, nil, nil)
			cancel()
			spans := exp.GetSpans()
			if len(spans) != 8 {
				t.Fatalf("expected task root, task span and 6 child spans, got %d", len(spans))
			}
			children := map[oteltrace.SpanID]bool{}
			for _, span := range spans {
				if span.SpanContext.TraceID() != spans[0].SpanContext.TraceID() {
					t.Fatal("task spans were split across traces")
				}
				if strings.HasPrefix(span.Name, "sandbox.") {
					if span.Parent.SpanID().String() != task.ID {
						t.Fatalf("%s is not under task", span.Name)
					}
					children[span.SpanContext.SpanID()] = true
				}
			}
			for _, span := range spans {
				if span.Name == "docker.http" && !children[span.Parent.SpanID()] {
					t.Fatal("Docker SDK span is not under sandbox operation")
				}
			}
		})
	}
}

func TestStartChildSpanSupportsOTelParentWithoutLangfuseTrace(t *testing.T) {
	m, exp := newTestManager(t)
	ctx, parent := m.Tracer().Start(context.Background(), "upstream")
	_, child := m.StartChildSpan(ctx, SpanOptions{Name: "sandbox.exec"})
	child.Finish(nil, nil, nil)
	parent.End()
	spans := exp.GetSpans()
	if len(spans) != 2 || spans[0].Parent.SpanID() != spans[1].SpanContext.SpanID() ||
		spans[0].SpanContext.TraceID() != spans[1].SpanContext.TraceID() {
		t.Fatal("OTel parent must be preserved without an extra automatic root")
	}
}

// spanAttr returns the string value of a span attribute, or "" if absent.
func spanAttr(attrs []attribute.KeyValue, key string) string {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			if v, ok := kv.Value.AsInterface().(string); ok {
				return v
			}
		}
	}
	return ""
}

// spanType returns the langfuse.observation.type of a span.
func spanType(s tracetest.SpanStub) string { return spanAttr(s.Attributes, attrObsType) }

func spanStringSliceAttr(attrs []attribute.KeyValue, key string) []string {
	for _, kv := range attrs {
		if string(kv.Key) == key && kv.Value.Type() == attribute.STRINGSLICE {
			return kv.Value.AsStringSlice()
		}
	}
	return nil
}

// TestSpan_NestedHierarchy verifies nested StartSpan calls produce a
// trace → span → span → generation tree with correct OTel parent linking
// (parenting is automatic through trace.SpanFromContext, no manual ids).
func TestSpan_NestedHierarchy(t *testing.T) {
	m, exp := newTestManager(t)

	ctx, trace := m.StartTrace(context.Background(), TraceOptions{Name: "root"})
	ctx, outer := m.StartSpan(ctx, SpanOptions{Name: "outer"})
	ctx, inner := m.StartSpan(ctx, SpanOptions{Name: "inner"})
	_, gen := m.StartGeneration(ctx, GenerationOptions{Name: "llm", Model: "m"})

	gen.Finish("out", &TokenUsage{Input: 1, Output: 2, Total: 3}, nil)
	inner.Finish("inner-out", nil, nil)
	outer.Finish("outer-out", nil, nil)
	trace.Finish("root-out", nil)

	spans := exp.GetSpans()
	if len(spans) != 4 {
		t.Fatalf("expected 4 spans, got %d", len(spans))
	}
	byName := map[string]tracetest.SpanStub{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	root, outerS, innerS, genS := byName["root"], byName["outer"], byName["inner"], byName["llm"]
	if root.Name == "" || outerS.Name == "" || innerS.Name == "" || genS.Name == "" {
		t.Fatalf("missing expected spans: %+v", byName)
	}
	// All spans share the trace id.
	if outerS.SpanContext.TraceID() != root.SpanContext.TraceID() ||
		innerS.SpanContext.TraceID() != root.SpanContext.TraceID() ||
		genS.SpanContext.TraceID() != root.SpanContext.TraceID() {
		t.Errorf("all spans must share the root trace id")
	}
	// Parent chain: outer → root, inner → outer, gen → inner.
	if outerS.Parent.SpanID() != root.SpanContext.SpanID() {
		t.Errorf("outer parent = %s, want root %s", outerS.Parent.SpanID(), root.SpanContext.SpanID())
	}
	if innerS.Parent.SpanID() != outerS.SpanContext.SpanID() {
		t.Errorf("inner parent = %s, want outer %s", innerS.Parent.SpanID(), outerS.SpanContext.SpanID())
	}
	if genS.Parent.SpanID() != innerS.SpanContext.SpanID() {
		t.Errorf("gen parent = %s, want inner %s", genS.Parent.SpanID(), innerS.SpanContext.SpanID())
	}
	// Observation types. A Langfuse trace is represented by a valid root
	// observation (span); "trace" is not a v4 observation type.
	if spanType(root) != obsTypeSpan {
		t.Errorf("root type = %q, want %q", spanType(root), obsTypeSpan)
	}
	if spanType(outerS) != obsTypeSpan || spanType(innerS) != obsTypeSpan {
		t.Errorf("outer/inner type not span: %q %q", spanType(outerS), spanType(innerS))
	}
	if spanType(genS) != obsTypeGeneration {
		t.Errorf("gen type = %q, want %q", spanType(genS), obsTypeGeneration)
	}
}

// TestSpan_FinishWithError records an error status on the span so failures in
// asynq handlers surface as red observations in Langfuse.
func TestSpan_FinishWithError(t *testing.T) {
	m, exp := newTestManager(t)

	ctx, _ := m.StartTrace(context.Background(), TraceOptions{Name: "root"})
	_, span := m.StartSpan(ctx, SpanOptions{Name: "boom"})
	span.Finish(nil, nil, errors.New("kaboom"))

	for _, s := range exp.GetSpans() {
		if s.Name != "boom" {
			continue
		}
		if s.Status.Code != codes.Error {
			t.Errorf("span status = %v, want Error", s.Status.Code)
		}
		if s.Status.Description != "kaboom" {
			t.Errorf("status description = %q, want kaboom", s.Status.Description)
		}
		return
	}
	t.Fatal("boom span not exported")
}

// TestManager_FullRoundTrip asserts a generation carries the model name and
// usage_details attribute (JSON with token counts), and shares the trace id
// of the root span.
func TestManager_FullRoundTrip(t *testing.T) {
	m, exp := newTestManager(t)

	ctx, trace := m.StartTrace(context.Background(), TraceOptions{Name: "test.trace", UserID: "user-42"})
	_, gen := m.StartGeneration(ctx, GenerationOptions{
		Name:  "chat.completion",
		Model: "gpt-test",
		Input: []map[string]string{{"role": "user", "content": "hi"}},
	})
	gen.Finish("hello", &TokenUsage{Input: 10, Output: 20, Total: 30, Unit: "TOKENS"}, nil)
	trace.Finish("hello", nil)

	for _, s := range exp.GetSpans() {
		if s.Name != "chat.completion" {
			continue
		}
		if spanAttr(s.Attributes, attrObsModel) != "gpt-test" {
			t.Errorf("model = %q, want gpt-test", spanAttr(s.Attributes, attrObsModel))
		}
		usage := spanAttr(s.Attributes, attrObsUsageDetails)
		if !strings.Contains(usage, `"total":30`) {
			t.Errorf("usage_details = %q, want total:30", usage)
		}
		if s.SpanContext.TraceID().String() != trace.ID {
			t.Errorf("gen trace id = %s, want %s", s.SpanContext.TraceID(), trace.ID)
		}
		return
	}
	t.Fatal("generation span not exported")
}

// TestSpan_FinishMetadataMerged verifies that metadata supplied at Finish is
// merged into (not discarded, nor overwriting) the metadata set at StartSpan.
// Regression guard: several call sites only know key fields (outcome,
// duration_ms, …) at completion and rely on Finish metadata being reported.
func TestSpan_FinishMetadataMerged(t *testing.T) {
	m, exp := newTestManager(t)

	ctx, tr := m.StartTrace(context.Background(), TraceOptions{Name: "root"})
	_, span := m.StartSpan(ctx, SpanOptions{
		Name:     "work",
		Metadata: map[string]interface{}{"stage": "ingest", "task_type": "manual"},
	})
	span.Finish("out", map[string]interface{}{"outcome": "success", "duration_ms": 42}, nil)
	tr.Finish(nil, nil)

	for _, s := range exp.GetSpans() {
		if s.Name != "work" {
			continue
		}
		meta := spanAttr(s.Attributes, attrObsMetadata)
		for _, want := range []string{`"stage":"ingest"`, `"task_type":"manual"`, `"outcome":"success"`, `"duration_ms":42`} {
			if !strings.Contains(meta, want) {
				t.Errorf("span metadata %q missing %q", meta, want)
			}
		}
		if got := spanAttr(s.Attributes, attrObsMetadata+".outcome"); got != "success" {
			t.Errorf("flat span outcome metadata = %q, want success", got)
		}
		return
	}
	t.Fatal("work span not exported")
}

// TestTrace_FinishMetadataMerged verifies the same merge behaviour on a trace:
// open-time correlation fields (request_id) survive alongside finish outcome.
func TestTrace_FinishMetadataMerged(t *testing.T) {
	m, exp := newTestManager(t)

	_, tr := m.StartTrace(context.Background(), TraceOptions{
		Name:     "root",
		Metadata: map[string]interface{}{"request_id": "req-1"},
	})
	tr.Finish("done", map[string]interface{}{"status": 200})

	for _, s := range exp.GetSpans() {
		if s.Name != "root" {
			continue
		}
		meta := spanAttr(s.Attributes, attrTraceMetadata)
		if !strings.Contains(meta, `"request_id":"req-1"`) || !strings.Contains(meta, `"status":200`) {
			t.Errorf("trace metadata %q missing merged fields", meta)
		}
		observationMeta := spanAttr(s.Attributes, attrObsMetadata)
		if !strings.Contains(observationMeta, `"request_id":"req-1"`) || !strings.Contains(observationMeta, `"status":200`) {
			t.Errorf("root observation metadata %q missing merged fields", observationMeta)
		}
		if got := spanAttr(s.Attributes, attrTraceMetadata+".status"); got != "200" {
			t.Errorf("flat trace status metadata = %q, want 200", got)
		}
		return
	}
	t.Fatal("root span not exported")
}

// TestStartGeneration_AutoRootExported guards the regression where a
// generation started with no active trace auto-opened a root span that was
// never ended and therefore never exported — leaving the generation's parent
// dangling. After the fix the auto root is exported and shares the trace id.
func TestStartGeneration_AutoRootExported(t *testing.T) {
	m, exp := newTestManager(t)

	_, gen := m.StartGeneration(context.Background(), GenerationOptions{Name: "llm", Model: "m"})
	gen.Finish("out", nil, nil)

	var root, generation tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		switch {
		case !s.Parent.IsValid():
			root = s
		case spanType(s) == obsTypeGeneration:
			generation = s
		}
	}
	if root.Name == "" {
		t.Fatal("auto-created root trace span was not exported")
	}
	if generation.Name == "" {
		t.Fatal("generation span was not exported")
	}
	if generation.SpanContext.TraceID() != root.SpanContext.TraceID() {
		t.Errorf("generation trace id %s != root trace id %s", generation.SpanContext.TraceID(), root.SpanContext.TraceID())
	}
	if generation.Parent.SpanID() != root.SpanContext.SpanID() {
		t.Errorf("generation parent %s != root span %s (dangling parent)", generation.Parent.SpanID(), root.SpanContext.SpanID())
	}
}

// TestStartSpan_AutoRootExported is the span counterpart of the above.
func TestStartSpan_AutoRootExported(t *testing.T) {
	m, exp := newTestManager(t)

	_, span := m.StartSpan(context.Background(), SpanOptions{Name: "orphan"})
	span.Finish("out", nil, nil)

	var sawRoot, sawSpan bool
	for _, s := range exp.GetSpans() {
		if !s.Parent.IsValid() {
			sawRoot = true
		}
		if s.Parent.IsValid() && spanType(s) == obsTypeSpan {
			sawSpan = true
		}
	}
	if !sawRoot {
		t.Error("auto-created root trace span was not exported")
	}
	if !sawSpan {
		t.Error("span was not exported")
	}
}

// TestTraceparentPropagation is the sop3 correlation core test: an incoming
// W3C traceparent (as injected by an upstream caller like sop3) is extracted,
// and the WeKnora root span inherits the upstream trace id — so in LiteFuse
// the WeKnora trace and the upstream caller's trace are the same trace.
func TestTraceparentPropagation(t *testing.T) {
	m, exp := newTestManager(t)

	// Simulate an upstream caller carrying a traceparent.
	upstreamCtx, upstreamSpan := m.Tracer().Start(context.Background(), "upstream-caller")
	remoteTraceID := upstreamSpan.SpanContext().TraceID()
	carrier := propagation.MapCarrier{}
	propagator.Inject(upstreamCtx, carrier)
	if carrier["traceparent"] == "" {
		t.Fatal("no traceparent injected")
	}

	// The HTTP middleware extracts the traceparent into the request context.
	httpCtx := propagator.Extract(context.Background(), carrier)
	_, trace := m.StartTrace(httpCtx, TraceOptions{Name: "weknora-root"})
	trace.Finish(nil, nil)

	for _, s := range exp.GetSpans() {
		if s.Name != "weknora-root" {
			continue
		}
		if s.SpanContext.TraceID() != remoteTraceID {
			t.Errorf("weknora root trace id = %s, want upstream %s (traceparent not inherited)",
				s.SpanContext.TraceID(), remoteTraceID)
		}
		return
	}
	t.Fatal("weknora-root span not exported")
}

// TestAttachTraceparent_FollowUpJoinsOriginatingTrace is the follow-up
// suggestion regression: generation often runs on a later HTTP request
// (POST .../suggestions) after the chat handler has already ended its root
// span. Without AttachTraceparent, StartGeneration auto-opens an orphan
// root named after the LLM call. With it, the follow-up span and generation
// inherit the originating chat trace id.
func TestAttachTraceparent_FollowUpJoinsOriginatingTrace(t *testing.T) {
	m, exp := newTestManager(t)

	httpCtx, httpTrace := m.StartTrace(context.Background(), TraceOptions{Name: "POST /api/v1/agent-chat/:session_id"})
	traceparent := TraceparentFromContext(httpCtx)
	if traceparent == "" {
		t.Fatal("expected a traceparent on the HTTP trace")
	}
	httpTrace.Finish(nil, nil)

	// Separate request: no *Trace, no OTel span — this is POST /suggestions.
	followCtx := AttachTraceparent(context.Background(), traceparent)
	followCtx, span := m.StartSpan(followCtx, SpanOptions{Name: "follow_up.suggestions"})
	_, gen := m.StartGeneration(followCtx, GenerationOptions{Name: "chat.completion", Model: "m"})
	gen.Finish("qs", nil, nil)
	span.Finish(nil, nil, nil)

	var followSpan, generation tracetest.SpanStub
	var autoRoot bool
	for _, s := range exp.GetSpans() {
		switch {
		case s.Name == "follow_up.suggestions" && spanType(s) == obsTypeSpan:
			followSpan = s
		case s.Name == "chat.completion" && spanType(s) == obsTypeGeneration:
			generation = s
		case s.Name == "chat.completion" && spanType(s) == obsTypeSpan && !s.Parent.IsValid():
			autoRoot = true
		}
	}
	if followSpan.Name == "" {
		t.Fatal("follow_up.suggestions span not exported")
	}
	if generation.Name == "" {
		t.Fatal("chat.completion generation not exported")
	}
	if followSpan.SpanContext.TraceID().String() != httpTrace.ID {
		t.Errorf("follow-up span trace id = %s, want HTTP %s", followSpan.SpanContext.TraceID(), httpTrace.ID)
	}
	if generation.SpanContext.TraceID().String() != httpTrace.ID {
		t.Errorf("generation trace id = %s, want HTTP %s", generation.SpanContext.TraceID(), httpTrace.ID)
	}
	if autoRoot {
		t.Error("StartGeneration opened an orphan chat.completion root; traceparent was not attached")
	}
}

// TestAttachTraceparent_LeavesExistingTrace ensures same-request background
// work (completeAssistantMessage) is not re-parented onto a stale remote
// span when ctx already carries a live *Trace.
func TestAttachTraceparent_LeavesExistingTrace(t *testing.T) {
	m, exp := newTestManager(t)

	otherCtx, other := m.StartTrace(context.Background(), TraceOptions{Name: "other"})
	otherParent := TraceparentFromContext(otherCtx)
	other.Finish(nil, nil)

	ctx, live := m.StartTrace(context.Background(), TraceOptions{Name: "live"})
	ctx = AttachTraceparent(ctx, otherParent)
	_, gen := m.StartGeneration(ctx, GenerationOptions{Name: "chat.completion", Model: "m"})
	gen.Finish("out", nil, nil)
	live.Finish(nil, nil)

	for _, s := range exp.GetSpans() {
		if spanType(s) != obsTypeGeneration {
			continue
		}
		if s.SpanContext.TraceID().String() != live.ID {
			t.Errorf("generation joined foreign trace %s, want live %s", s.SpanContext.TraceID(), live.ID)
		}
		return
	}
	t.Fatal("generation span not exported")
}

func TestTraceparentFromContext_ReestablishesDetachedTrace(t *testing.T) {
	m, _ := newTestManager(t)

	ctx, root := m.StartTrace(context.Background(), TraceOptions{Name: "root"})
	root.Finish(nil, nil)
	detached := context.WithValue(context.Background(), traceCtxKey, root)

	got := TraceparentFromContext(detached)
	if got == "" || !strings.HasPrefix(got, "00-"+root.ID) {
		t.Fatalf("traceparent from detached trace = %q, want trace id %s", got, root.ID)
	}

	// Keep ctx referenced so the test also verifies the returned start context
	// itself was the source of the trace handle before it was detached.
	if TraceparentFromContext(ctx) == "" {
		t.Fatal("traceparent missing from the original trace context")
	}
}

// TestV4TraceContextAndObservationTypes guards the attributes that make a
// direct OTLP trace useful in Langfuse v4: root observation I/O, valid
// semantic types, and trace-wide fields copied to child observations.
func TestV4TraceContextAndObservationTypes(t *testing.T) {
	m, exp := newTestManager(t)

	ctx, root := m.StartTrace(context.Background(), TraceOptions{
		Name:      "chat.turn",
		UserID:    "user-1",
		SessionID: "session-1",
		Input:     map[string]interface{}{"query": "hello"},
		Metadata: map[string]interface{}{
			"request_id": "request-1",
			"api_key":    "must-not-appear",
			"nested":     map[string]string{"authToken": "nested-secret"},
		},
		Tags: []string{"chat", "production"},
	})
	ctx, retriever := m.StartSpan(ctx, SpanOptions{
		Name:            "retrieve",
		ObservationType: obsTypeRetriever,
		Input:           map[string]interface{}{"query": "hello", "api_key": "input-secret"},
		Metadata:        map[string]interface{}{"retriever": "vector"},
	})
	_, embedding := m.StartGeneration(ctx, GenerationOptions{
		Name:            "embedding.embed",
		Model:           "embed-test",
		ObservationType: obsTypeEmbedding,
	})
	embedding.Finish(map[string]interface{}{"dimensions": 3}, nil, nil)
	retriever.Finish(map[string]interface{}{"hits": 1}, map[string]interface{}{"authToken": "finish-secret"}, nil)
	root.SetOutput(map[string]interface{}{"answer": "world"})
	root.Finish(map[string]interface{}{"status": 200}, map[string]interface{}{"outcome": "success"})

	var rootStub, retrieverStub, embeddingStub tracetest.SpanStub
	for _, span := range exp.GetSpans() {
		switch span.Name {
		case "chat.turn":
			rootStub = span
		case "retrieve":
			retrieverStub = span
		case "embedding.embed":
			embeddingStub = span
		}
	}
	if !rootStub.SpanContext.IsValid() || !retrieverStub.SpanContext.IsValid() || !embeddingStub.SpanContext.IsValid() {
		t.Fatalf("expected root and child observations, got %d spans", len(exp.GetSpans()))
	}
	if spanType(rootStub) != obsTypeSpan {
		t.Fatalf("root type = %q, want %q", spanType(rootStub), obsTypeSpan)
	}
	if spanType(retrieverStub) != obsTypeRetriever {
		t.Fatalf("retriever type = %q, want %q", spanType(retrieverStub), obsTypeRetriever)
	}
	if spanType(embeddingStub) != obsTypeEmbedding {
		t.Fatalf("embedding type = %q, want %q", spanType(embeddingStub), obsTypeEmbedding)
	}
	if input := spanAttr(rootStub.Attributes, attrObsInput); !strings.Contains(input, `"query":"hello"`) {
		t.Fatalf("root observation input = %q, want user query", input)
	}
	retrieverInput := spanAttr(retrieverStub.Attributes, attrObsInput)
	if strings.Contains(retrieverInput, "input-secret") {
		t.Fatalf("sensitive observation input leaked into trace: %q", retrieverInput)
	}
	retrieverMetadata := spanAttr(retrieverStub.Attributes, attrObsMetadata)
	if strings.Contains(retrieverMetadata, "finish-secret") {
		t.Fatalf("sensitive finish metadata leaked into trace: %q", retrieverMetadata)
	}
	output := spanAttr(rootStub.Attributes, attrObsOutput)
	if !strings.Contains(output, `"answer":"world"`) || !strings.Contains(output, `"status":200`) {
		t.Fatalf("root observation output = %q, want answer and status", output)
	}
	for _, span := range []tracetest.SpanStub{rootStub, retrieverStub, embeddingStub} {
		if spanAttr(span.Attributes, attrLangfuseUserID) != "user-1" ||
			spanAttr(span.Attributes, attrLangfuseSessionID) != "session-1" {
			t.Fatalf("trace identity missing from %s", span.Name)
		}
		if spanAttr(span.Attributes, attrTraceName) != "chat.turn" {
			t.Fatalf("trace name missing from %s", span.Name)
		}
		if got := spanStringSliceAttr(span.Attributes, attrTraceTags); len(got) != 2 {
			t.Fatalf("trace tags on %s = %#v, want two tags", span.Name, got)
		}
		if spanAttr(span.Attributes, attrTraceMetadata+".request_id") != "request-1" {
			t.Fatalf("request metadata missing from %s", span.Name)
		}
	}
	metadata := spanAttr(rootStub.Attributes, attrTraceMetadata)
	if strings.Contains(metadata, "must-not-appear") || strings.Contains(metadata, "nested-secret") {
		t.Fatalf("sensitive metadata leaked into trace: %q", metadata)
	}
}
