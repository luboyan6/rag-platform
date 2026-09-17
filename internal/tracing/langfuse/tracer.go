package langfuse

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Trace represents an active root observation. A Trace is conceptually one
// "request" (e.g. a chat turn). Generations and spans attached to it roll up
// as children in the Langfuse UI. It wraps an OpenTelemetry root span; its
// ID is the OTel trace id (W3C 32-hex), which — when the request carried a
// traceparent header — is the upstream caller's trace id (sop3 correlation).
type Trace struct {
	ID          string
	span        trace.Span
	manager     *Manager
	mu          sync.Mutex
	finished    bool
	input       interface{}
	output      interface{}
	name        string
	userID      string
	sessionID   string
	tags        []string
	environment string
	release     string
	// metadata holds the metadata set at StartTrace so Finish can merge (not
	// overwrite) the finish-time metadata into it before serializing.
	metadata map[string]interface{}
}

// Generation represents a single model invocation (LLM / embedding / VLM / ASR).
type Generation struct {
	ID      string
	span    trace.Span
	manager *Manager
	model   string
	name    string
	// autoTrace is a non-nil root trace this generation implicitly opened
	// because ctx carried none; Finish must End it so the root is exported.
	autoTrace *Trace
}

// Span represents a logical unit of work that isn't itself an LLM call — for
// example an asynq task execution, a pipeline stage, or a document-processing
// step. Generations and nested spans attach as children via the OTel span
// context (parenting is automatic through trace.SpanFromContext).
type Span struct {
	ID      string
	span    trace.Span
	manager *Manager
	name    string
	// metadata holds the metadata set at StartSpan so Finish can merge (not
	// overwrite) the finish-time metadata into it before serializing.
	metadata map[string]interface{}
	// autoTrace is a non-nil root trace this span implicitly opened because
	// ctx carried none; Finish must End it so the root is exported.
	autoTrace *Trace
}

// TraceOptions configures a new trace.
type TraceOptions struct {
	Name        string
	UserID      string
	SessionID   string
	Input       interface{}
	Metadata    map[string]interface{}
	Tags        []string
	Environment string
	Release     string
}

// GenerationOptions configures a new generation observation.
type GenerationOptions struct {
	Name            string
	Model           string
	Input           interface{}
	Metadata        map[string]interface{}
	ModelParameters map[string]interface{}
	// ObservationType defaults to generation. Embeddings use the more
	// specific "embedding" type while still retaining model/usage fields.
	ObservationType string
}

// SpanOptions configures a new SPAN observation.
type SpanOptions struct {
	Name     string
	Input    interface{}
	Metadata map[string]interface{}
	// ObservationType defaults to span. Use the most specific Langfuse type
	// available for semantic operations such as agent, tool, chain, or
	// retriever.
	ObservationType string
}

// StartTrace opens a root span. When ctx carries a remote SpanContext (from a
// W3C traceparent extracted by GinMiddleware), the root span inherits the
// upstream trace id — this is what makes a sop3 run and its WeKnora call land
// under the same trace in LiteFuse. The returned *Trace is non-nil even when
// disabled (methods are no-ops), so callers don't need nil checks.
func (m *Manager) StartTrace(ctx context.Context, opts TraceOptions) (context.Context, *Trace) {
	if !m.Enabled() {
		return ctx, &Trace{manager: m}
	}
	name := opts.Name
	if strings.TrimSpace(name) == "" {
		name = "request"
	}
	metadata := sanitizeMetadata(opts.Metadata)
	tags := normalizeTags(opts.Tags)
	env := opts.Environment
	if env == "" {
		env = m.cfg.Environment
	}
	rel := opts.Release
	if rel == "" {
		rel = m.cfg.Release
	}
	t := &Trace{
		manager:     m,
		name:        name,
		userID:      opts.UserID,
		sessionID:   opts.SessionID,
		tags:        tagsCopy(tags),
		environment: env,
		release:     rel,
		input:       opts.Input,
		metadata:    metadata,
	}
	ctx = withTraceBaggage(ctx, m, t)
	attrs := []attribute.KeyValue{attribute.String(attrObsType, obsTypeSpan)}
	attrs = append(attrs, attribute.String(attrTraceName, name))
	if opts.UserID != "" {
		attrs = append(attrs,
			attribute.String(attrLangfuseUserID, opts.UserID),
			attribute.String(attrUserID, opts.UserID),
		)
	}
	if opts.SessionID != "" {
		attrs = append(attrs,
			attribute.String(attrLangfuseSessionID, opts.SessionID),
			attribute.String(attrSessionID, opts.SessionID),
		)
	}
	if env != "" {
		attrs = append(attrs, attribute.String(attrEnvironment, env))
	}
	if rel != "" {
		attrs = append(attrs, attribute.String(attrRelease, rel))
	}
	if opts.Input != nil {
		// v4 reads root input/output from the observation attributes. Keep the
		// old trace keys during the migration for existing self-hosted data.
		attrs = append(attrs,
			jsonAttr(attrObsInput, opts.Input),
			jsonAttr(attrTraceInput, opts.Input),
		)
	}
	if len(metadata) > 0 {
		attrs = append(attrs,
			jsonAttr(attrObsMetadata, metadata),
			jsonAttr(attrTraceMetadata, metadata),
		)
		attrs = append(attrs, flatMetadataAttributes(attrObsMetadata, metadata)...)
		attrs = append(attrs, flatMetadataAttributes(attrTraceMetadata, metadata)...)
	}
	if len(tags) > 0 {
		attrs = append(attrs, attribute.StringSlice(attrTraceTags, tags))
	}
	ctx, span := m.tracer.Start(ctx, name, trace.WithTimestamp(time.Now()), trace.WithAttributes(attrs...))
	t.ID = span.SpanContext().TraceID().String()
	t.span = span
	return withTrace(ctx, t), t
}

// Finish updates the trace with its final output and merges any finish-time
// metadata into the metadata set at StartTrace. Safe to call on a disabled
// trace (no-op). Finish keys are merged on top of the open-time correlation
// fields (request_id, http.method, etc.) rather than overwriting them, so
// both the open's correlation and the finish outcome survive.
func (t *Trace) Finish(output interface{}, metadata map[string]interface{}) {
	if t == nil || t.manager == nil || !t.manager.Enabled() || t.span == nil {
		return
	}
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	t.finished = true
	if output != nil {
		t.output = mergeTraceValues(t.output, output)
	}
	finalOutput := t.output
	merged := mergeMetadata(t.metadata, sanitizeMetadata(metadata))
	span := t.span
	t.mu.Unlock()

	attrs := make([]attribute.KeyValue, 0, 4)
	if finalOutput != nil {
		attrs = append(attrs,
			jsonAttr(attrObsOutput, finalOutput),
			jsonAttr(attrTraceOutput, finalOutput),
		)
	}
	if merged != nil {
		attrs = append(attrs,
			jsonAttr(attrObsMetadata, merged),
			jsonAttr(attrTraceMetadata, merged),
		)
		attrs = append(attrs, flatMetadataAttributes(attrObsMetadata, merged)...)
		attrs = append(attrs, flatMetadataAttributes(attrTraceMetadata, merged)...)
	}
	span.SetAttributes(attrs...)
	span.End()
}

// SetInput updates the meaningful application input on the root observation.
// Handlers use this after request validation so the trace contains the user
// query rather than the raw HTTP body or framework arguments.
func (t *Trace) SetInput(input interface{}) {
	if t == nil || t.manager == nil || !t.manager.Enabled() || t.span == nil {
		return
	}
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	t.input = input
	span := t.span
	t.mu.Unlock()
	span.SetAttributes(jsonAttr(attrObsInput, input), jsonAttr(attrTraceInput, input))
}

// SetOutput updates the meaningful application output on the root
// observation. A later Finish call merges map outputs so HTTP status and the
// assistant answer can coexist in the Langfuse Output pane.
func (t *Trace) SetOutput(output interface{}) {
	if t == nil || t.manager == nil || !t.manager.Enabled() || t.span == nil {
		return
	}
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	t.output = mergeTraceValues(t.output, output)
	finalOutput := t.output
	span := t.span
	t.mu.Unlock()
	span.SetAttributes(jsonAttr(attrObsOutput, finalOutput), jsonAttr(attrTraceOutput, finalOutput))
}

// ResumeTrace reconstructs a *Trace handle from an externally-provided W3C
// trace id (and optional parent span id), without creating a new root span —
// the originating process (e.g. an HTTP request that already opened a trace)
// owns the root. Used to graft async work onto an existing trace: it sets a
// remote SpanContext on ctx so any child span/generation started under it
// inherits the upstream trace id. When traceID is empty the returned *Trace
// is nil, signalling the caller should fall back to StartTrace.
func (m *Manager) ResumeTrace(ctx context.Context, traceID, parentSpanID string) (context.Context, *Trace) {
	if m == nil || !m.Enabled() || traceID == "" {
		return ctx, nil
	}
	tid, err := trace.TraceIDFromHex(traceID)
	if err != nil {
		// Not a W3C 32-hex trace id (legacy UUID, etc.); cannot resume.
		return ctx, nil
	}
	var sid trace.SpanID
	if parentSpanID != "" {
		if s, err := trace.SpanIDFromHex(parentSpanID); err == nil {
			sid = s
		}
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
	t := &Trace{
		ID:        traceID,
		manager:   m,
		userID:    userIDFromCtx(ctx),
		sessionID: sessionIDFromCtx(ctx),
	}
	ctx = withTraceBaggage(ctx, m, t)
	return withTrace(ctx, t), t
}

// reestablishParentSpan re-injects the active trace's root span as the OTel
// parent when ctx carries a *Trace but no active OTel span. This happens when
// a context rebuild drops the OTel span while the *Trace handle survives on
// the exported key (e.g. a background goroutine derived from a non-request
// context, or a CloneContext that predates/missed the span fix). Without
// this, child spans (e.g. a summary generation) start a fresh root and orphan
// off the HTTP trace.
func (m *Manager) reestablishParentSpan(ctx context.Context) context.Context {
	if !m.Enabled() {
		return ctx
	}
	if sp := trace.SpanFromContext(ctx); sp.IsRecording() {
		return ctx // already has an active span
	}
	if t, ok := traceFromCtx(ctx); ok && t != nil && t.span != nil {
		return trace.ContextWithSpan(ctx, t.span)
	}
	return ctx
}

// StartSpan opens a child span under the trace/span carried by ctx. When no
// trace is present, OTel creates a fresh root (mirroring StartGeneration's
// auto-trace behaviour). Returns a ctx whose active span is this span.
func (m *Manager) StartSpan(ctx context.Context, opts SpanOptions) (context.Context, *Span) {
	return m.startSpan(ctx, opts, true)
}

// StartChildSpan records a low-level operation only when its caller is already
// traced. Polling and housekeeping must not create a new trace for every RPC;
// their operation-level caller owns the trace boundary instead.
func (m *Manager) StartChildSpan(ctx context.Context, opts SpanOptions) (context.Context, *Span) {
	ctx = m.reestablishParentSpan(ctx)
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx, &Span{manager: m}
	}
	return m.startSpan(ctx, opts, false)
}

func (m *Manager) startSpan(ctx context.Context, opts SpanOptions, createTrace bool) (context.Context, *Span) {
	if !m.Enabled() {
		return ctx, &Span{manager: m}
	}
	ctx = m.reestablishParentSpan(ctx)
	var autoTrace *Trace
	if _, ok := traceFromCtx(ctx); !ok && createTrace {
		// No active trace: open a shallow root so the span isn't orphaned.
		// Hold the handle so Finish can End it — otherwise the root span is
		// never exported and this span's parent points at a missing span.
		ctx, autoTrace = m.StartTrace(ctx, TraceOptions{Name: opts.Name})
	}
	if t, ok := traceFromCtx(ctx); ok {
		ctx = withTraceBaggage(ctx, m, t)
	}
	name := opts.Name
	if strings.TrimSpace(name) == "" {
		name = "span"
	}
	metadata := sanitizeMetadata(opts.Metadata)
	attrs := []attribute.KeyValue{
		attribute.String(attrObsType, observationType(opts.ObservationType, obsTypeSpan)),
		jsonAttr(attrObsInput, opts.Input),
		jsonAttr(attrObsMetadata, metadata),
	}
	attrs = append(attrs, flatMetadataAttributes(attrObsMetadata, metadata)...)
	ctx, span := m.tracer.Start(ctx, name, trace.WithTimestamp(time.Now()), trace.WithAttributes(attrs...))
	return ctx, &Span{
		ID:        span.SpanContext().SpanID().String(),
		span:      span,
		manager:   m,
		name:      name,
		metadata:  metadata,
		autoTrace: autoTrace,
	}
}

// Finish updates a span with its final output, extra metadata and any error.
// A non-nil err marks the span as ERROR. Finish-time metadata is merged on top
// of the metadata set at StartSpan (finish keys win) rather than discarded, so
// fields only known at completion (outcome, duration_ms, tool_calls, …) are
// reported. If this span implicitly opened a root trace, that root is ended
// last so it is exported.
func (s *Span) Finish(output interface{}, metadata map[string]interface{}, err error) {
	if s == nil || s.manager == nil || !s.manager.Enabled() || s.span == nil {
		return
	}
	attrs := []attribute.KeyValue{jsonAttr(attrObsOutput, output)}
	if merged := mergeMetadata(s.metadata, sanitizeMetadata(metadata)); merged != nil {
		attrs = append(attrs, jsonAttr(attrObsMetadata, merged))
		attrs = append(attrs, flatMetadataAttributes(attrObsMetadata, merged)...)
	}
	s.span.SetAttributes(attrs...)
	if err != nil {
		s.span.RecordError(err)
		s.span.SetStatus(codes.Error, err.Error())
	}
	s.span.End()
	if s.autoTrace != nil {
		s.autoTrace.Finish(nil, nil)
	}
}

// StartGeneration opens a generation observation under the trace carried by
// ctx (or a newly auto-created trace). If a parent span is present on ctx,
// the generation attaches under it via the OTel span context.
func (m *Manager) StartGeneration(ctx context.Context, opts GenerationOptions) (context.Context, *Generation) {
	if !m.Enabled() {
		return ctx, &Generation{manager: m, model: opts.Model, name: opts.Name}
	}
	ctx = m.reestablishParentSpan(ctx)
	var autoTrace *Trace
	if _, ok := traceFromCtx(ctx); !ok {
		// No active trace: open a root so the generation isn't orphaned, and
		// hold the handle so Finish can End it (otherwise the root span never
		// gets exported and this generation's parent points at nothing).
		ctx, autoTrace = m.StartTrace(ctx, TraceOptions{Name: opts.Name})
	}
	if t, ok := traceFromCtx(ctx); ok {
		ctx = withTraceBaggage(ctx, m, t)
	}
	name := opts.Name
	if strings.TrimSpace(name) == "" {
		name = "generation"
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = "unknown"
	}
	metadata := sanitizeMetadata(opts.Metadata)
	attrs := []attribute.KeyValue{
		attribute.String(attrObsType, observationType(opts.ObservationType, obsTypeGeneration)),
		attribute.String(attrObsModel, model),
		jsonAttr(attrObsInput, opts.Input),
		jsonAttr(attrObsMetadata, metadata),
		jsonAttr(attrObsModelParams, opts.ModelParameters),
	}
	attrs = append(attrs, flatMetadataAttributes(attrObsMetadata, metadata)...)
	ctx, span := m.tracer.Start(ctx, name, trace.WithTimestamp(time.Now()), trace.WithAttributes(attrs...))
	g := &Generation{
		ID:        span.SpanContext().SpanID().String(),
		span:      span,
		manager:   m,
		model:     model,
		name:      name,
		autoTrace: autoTrace,
	}
	return ctx, g
}

// Finish updates a generation with its final output, token usage and any
// error. A non-nil err marks the observation as ERROR.
func (g *Generation) Finish(output interface{}, usage *TokenUsage, err error) {
	if g == nil || g.manager == nil || !g.manager.Enabled() || g.span == nil {
		return
	}
	attrs := []attribute.KeyValue{jsonAttr(attrObsOutput, output)}
	if usage != nil {
		attrs = append(attrs, jsonAttr(attrObsUsageDetails, usage))
	}
	g.span.SetAttributes(attrs...)
	if err != nil {
		g.span.RecordError(err)
		g.span.SetStatus(codes.Error, err.Error())
	}
	g.span.End()
	if g.autoTrace != nil {
		g.autoTrace.Finish(nil, nil)
	}
}

// MarkCompletionStart records the time at which the first token was received
// in a streaming generation. Langfuse surfaces this as time-to-first-token.
func (g *Generation) MarkCompletionStart(t time.Time) {
	if g == nil || g.manager == nil || !g.manager.Enabled() || g.span == nil {
		return
	}
	g.span.SetAttributes(attribute.String(attrObsCompletionStart, isoTime(t)))
}

// mergeMetadata combines the metadata captured when an observation opened
// with the metadata supplied at Finish. Finish keys win on conflict (they
// reflect the final outcome), while open-time keys (correlation fields such
// as request_id / http.method) are preserved. Returns nil when both inputs
// are empty so callers can skip writing an empty attribute.
func mergeMetadata(start, finish map[string]interface{}) map[string]interface{} {
	if len(start) == 0 && len(finish) == 0 {
		return nil
	}
	merged := make(map[string]interface{}, len(start)+len(finish))
	for k, v := range start {
		merged[k] = v
	}
	for k, v := range finish {
		merged[k] = v
	}
	return merged
}

func mergeTraceValues(existing, update interface{}) interface{} {
	if existing == nil {
		return update
	}
	if update == nil {
		return existing
	}
	left, leftOK := existing.(map[string]interface{})
	right, rightOK := update.(map[string]interface{})
	if !leftOK || !rightOK {
		return update
	}
	merged := make(map[string]interface{}, len(left)+len(right))
	for key, value := range left {
		merged[key] = value
	}
	for key, value := range right {
		merged[key] = value
	}
	return merged
}

// jsonAttr serializes v to a compact JSON string and wraps it as a string
// OTel attribute — matching how langfuse-python stores structured fields
// (input/output/metadata/usage) on spans. nil/zero values return an empty
// KeyValue (harmless on SetAttributes).
func jsonAttr(key string, v interface{}) attribute.KeyValue {
	if v == nil {
		return attribute.KeyValue{Key: attribute.Key(key)}
	}
	v = sanitizeTraceValue(v)
	b, err := json.Marshal(v)
	if err != nil {
		logger.Warnf(context.Background(), "[Langfuse] marshal attr %s failed: %v", key, err)
		return attribute.KeyValue{Key: attribute.Key(key)}
	}
	if len(b) == 0 || string(b) == "null" {
		// Optional structured fields are often unset; omit rather than warn.
		return attribute.KeyValue{Key: attribute.Key(key)}
	}
	return attribute.String(key, string(b))
}
