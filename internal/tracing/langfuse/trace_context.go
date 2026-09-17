package langfuse

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Langfuse v4 expects trace-wide attributes on every observation that may be
// queried or aggregated. OpenTelemetry baggage is used only as an in-process
// carrier here: the package's propagator intentionally remains TraceContext,
// so these values are not sent to an arbitrary upstream service.
const (
	baggageTraceName      = "weknora.langfuse.trace_name"
	baggageUserID         = "weknora.langfuse.user_id"
	baggageSessionID      = "weknora.langfuse.session_id"
	baggageEnvironment    = "weknora.langfuse.environment"
	baggageRelease        = "weknora.langfuse.release"
	baggageTags           = "weknora.langfuse.tags"
	baggageTraceMetadata  = "weknora.langfuse.trace_metadata"
	maxBaggageMetadataLen = 4096
	maxMetadataValueLen   = 1024
	maxTraceValueLen      = 16 * 1024
	maxTraceValueDepth    = 6
	maxTraceMapItems      = 64
	maxTraceArrayItems    = 64
)

// langfuseBaggageSpanProcessor copies the selected trace context from the
// parent context onto each newly-created span. This is the Go equivalent of
// the BaggageSpanProcessor recommended by Langfuse's v4 OpenTelemetry guide.
// It is deliberately synchronous and allocation-light because OnStart runs on
// the caller's hot path.
type langfuseBaggageSpanProcessor struct {
	environment string
	release     string
}

func newLangfuseBaggageSpanProcessor(environment, release string) sdktrace.SpanProcessor {
	return &langfuseBaggageSpanProcessor{
		environment: environment,
		release:     release,
	}
}

func (p *langfuseBaggageSpanProcessor) OnStart(parent context.Context, span sdktrace.ReadWriteSpan) {
	if parent == nil || span == nil {
		return
	}

	b := baggage.FromContext(parent)
	attrs := make([]attribute.KeyValue, 0, 8)
	appendIdentity := func(value, canonical, legacy string) {
		if value == "" {
			return
		}
		attrs = append(attrs,
			attribute.String(canonical, value),
			attribute.String(legacy, value),
		)
	}
	appendIdentity(b.Member(baggageUserID).Value(), attrLangfuseUserID, attrUserID)
	appendIdentity(b.Member(baggageSessionID).Value(), attrLangfuseSessionID, attrSessionID)

	if name := b.Member(baggageTraceName).Value(); name != "" {
		attrs = append(attrs, attribute.String(attrTraceName, name))
	}
	environment := b.Member(baggageEnvironment).Value()
	if environment == "" {
		environment = p.environment
	}
	if environment != "" {
		attrs = append(attrs, attribute.String(attrEnvironment, environment))
	}
	release := b.Member(baggageRelease).Value()
	if release == "" {
		release = p.release
	}
	if release != "" {
		attrs = append(attrs, attribute.String(attrRelease, release))
	}

	if tags := decodeTags(b.Member(baggageTags).Value()); len(tags) > 0 {
		attrs = append(attrs, attribute.StringSlice(attrTraceTags, tags))
	}
	attrs = append(attrs, flatMetadataAttributes(attrTraceMetadata,
		decodeMetadata(b.Member(baggageTraceMetadata).Value()))...)
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
}

func (p *langfuseBaggageSpanProcessor) OnEnd(sdktrace.ReadOnlySpan) {}

func (p *langfuseBaggageSpanProcessor) Shutdown(context.Context) error { return nil }

func (p *langfuseBaggageSpanProcessor) ForceFlush(context.Context) error { return nil }

// withTraceBaggage stores only low-cardinality trace context and sanitized
// metadata. The context is intentionally rebuilt from the Trace handle each
// time a child starts because logger.CloneContext preserves the handle and OTel
// span but cannot know about this package's baggage value.
func withTraceBaggage(ctx context.Context, manager *Manager, t *Trace) context.Context {
	if ctx == nil || manager == nil || t == nil || !manager.Enabled() {
		return ctx
	}

	environment := t.environment
	if environment == "" {
		environment = manager.cfg.Environment
	}
	release := t.release
	if release == "" {
		release = manager.cfg.Release
	}
	return withTraceBaggageValues(ctx, manager, t.name, t.userID, t.sessionID,
		tagsCopy(t.tags), environment, release, t.metadata)
}

func withTraceBaggageValues(
	ctx context.Context,
	manager *Manager,
	name, userID, sessionID string,
	tags []string, environment, release string,
	metadata map[string]interface{},
) context.Context {
	if ctx == nil || manager == nil || !manager.Enabled() {
		return ctx
	}

	b := baggage.FromContext(ctx)
	setMember := func(key, value string) {
		if value == "" {
			b = b.DeleteMember(key)
			return
		}
		member, err := baggage.NewMemberRaw(key, value)
		if err != nil {
			return
		}
		if next, err := b.SetMember(member); err == nil {
			b = next
		}
	}
	setMember(baggageTraceName, name)
	setMember(baggageUserID, userID)
	setMember(baggageSessionID, sessionID)
	setMember(baggageEnvironment, environment)
	setMember(baggageRelease, release)
	if normalizedTags := normalizeTags(tags); len(normalizedTags) > 0 {
		if encoded, err := json.Marshal(normalizedTags); err == nil {
			setMember(baggageTags, string(encoded))
		}
	} else {
		setMember(baggageTags, "")
	}
	if encoded := encodeTraceMetadata(metadata); encoded != "" {
		setMember(baggageTraceMetadata, encoded)
	} else {
		setMember(baggageTraceMetadata, "")
	}
	return baggage.ContextWithBaggage(ctx, b)
}

func decodeTags(encoded string) []string {
	if encoded == "" {
		return nil
	}
	var tags []string
	if json.Unmarshal([]byte(encoded), &tags) != nil {
		return nil
	}
	return normalizeTags(tags)
}

func decodeMetadata(encoded string) map[string]interface{} {
	if encoded == "" {
		return nil
	}
	var metadata map[string]interface{}
	if json.Unmarshal([]byte(encoded), &metadata) != nil {
		return nil
	}
	return metadata
}

func encodeTraceMetadata(metadata map[string]interface{}) string {
	metadata = sanitizeMetadata(metadata)
	if len(metadata) == 0 {
		return ""
	}
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		if safeMetadataKey(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	ordered := make(map[string]interface{}, len(keys))
	for _, key := range keys {
		ordered[key] = metadata[key]
	}
	encoded, err := json.Marshal(ordered)
	if err != nil || len(encoded) > maxBaggageMetadataLen {
		return ""
	}
	return string(encoded)
}

func flatMetadataAttributes(prefix string, metadata map[string]interface{}) []attribute.KeyValue {
	if len(metadata) == 0 {
		return nil
	}
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		if safeMetadataKey(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	attrs := make([]attribute.KeyValue, 0, len(keys))
	for _, key := range keys {
		value := metadataAttributeValue(metadata[key])
		if value == "" {
			continue
		}
		attrs = append(attrs, attribute.String(prefix+"."+key, value))
	}
	return attrs
}

func metadataAttributeValue(value interface{}) string {
	if value == nil {
		return ""
	}
	// Metadata values can be typed structs/slices supplied by call sites. The
	// map-level sanitizer handles the common JSON-shaped cases, but applying
	// the same bounded/redacted normalization here closes that gap for the
	// flattened, filterable attribute path as well.
	value = sanitizeTraceValue(value)
	if s, ok := value.(string); ok {
		return s
	}
	encoded, err := json.Marshal(value)
	if err != nil || string(encoded) == "null" {
		return ""
	}
	return string(encoded)
}

func sanitizeMetadata(metadata map[string]interface{}) map[string]interface{} {
	return sanitizeMetadataMap(metadata, 0)
}

// sanitizeTraceValue applies the same field-level redaction and bounded-value
// policy to structured input/output attributes. The JSON round-trip lets this
// cover typed slices and structs (chat messages, tool calls, provider
// responses) without reflection-heavy code in every model wrapper.
func sanitizeTraceValue(value interface{}) interface{} {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var decoded interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return value
	}
	return sanitizeTraceValueNode(decoded, 0)
}

func sanitizeTraceValueNode(value interface{}, depth int) interface{} {
	if depth >= maxTraceValueDepth {
		return "[TRUNCATED]"
	}
	switch value := value.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, minInt(len(value), maxTraceMapItems))
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for i, key := range keys {
			if i >= maxTraceMapItems {
				break
			}
			if sensitiveField(key) {
				out[key] = "[REDACTED]"
				continue
			}
			out[key] = sanitizeTraceValueNode(value[key], depth+1)
		}
		return out
	case []interface{}:
		out := make([]interface{}, 0, minInt(len(value), maxTraceArrayItems))
		for i, item := range value {
			if i >= maxTraceArrayItems {
				break
			}
			out = append(out, sanitizeTraceValueNode(item, depth+1))
		}
		return out
	case string:
		return truncateTraceString(value, maxTraceValueLen)
	default:
		return value
	}
}

func sanitizeMetadataMap(metadata map[string]interface{}, depth int) map[string]interface{} {
	if len(metadata) == 0 {
		return nil
	}
	clean := make(map[string]interface{}, len(metadata))
	for key, value := range metadata {
		if sensitiveField(key) {
			clean[key] = "[REDACTED]"
			continue
		}
		clean[key] = sanitizeMetadataValue(value, depth+1)
	}
	return clean
}

func sanitizeMetadataValue(value interface{}, depth int) interface{} {
	if depth >= 4 {
		return "[TRUNCATED]"
	}
	switch value := value.(type) {
	case string:
		return truncateTraceString(value, maxMetadataValueLen)
	case map[string]interface{}:
		return sanitizeMetadataMap(value, depth+1)
	case map[string]string:
		out := make(map[string]interface{}, len(value))
		for key, item := range value {
			if sensitiveField(key) {
				out[key] = "[REDACTED]"
				continue
			}
			out[key] = truncateTraceString(item, maxMetadataValueLen)
		}
		return out
	case []interface{}:
		out := make([]interface{}, 0, minInt(len(value), 32))
		for i, item := range value {
			if i >= 32 {
				break
			}
			out = append(out, sanitizeMetadataValue(item, depth+1))
		}
		return out
	case []string:
		out := make([]string, 0, minInt(len(value), 32))
		for i, item := range value {
			if i >= 32 {
				break
			}
			out = append(out, truncateTraceString(item, maxMetadataValueLen))
		}
		return out
	default:
		return value
	}
}

func safeMetadataKey(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 64 || sensitiveField(key) {
		return false
	}
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func sensitiveField(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	compact := strings.NewReplacer("_", "", ".", "").Replace(normalized)
	for _, sensitive := range []string{
		"password", "passwd", "secret", "api_key", "apikey", "authorization", "cookie",
		"access_token", "refresh_token", "id_token", "client_secret", "private_key", "auth_token",
	} {
		compactSensitive := strings.ReplaceAll(sensitive, "_", "")
		if normalized == sensitive || strings.Contains(normalized, sensitive) ||
			compact == compactSensitive || strings.Contains(compact, compactSensitive) {
			return true
		}
	}
	switch normalized {
	case "token", "code", "state", "ticket":
		return true
	default:
		return false
	}
}

func truncateTraceString(value string, maxRunes int) string {
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "..."
}

func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, 0, minInt(len(tags), 32))
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		tag = truncateTraceString(strings.TrimSpace(tag), 64)
		if tag == "" || sensitiveField(tag) {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
		if len(out) == 32 {
			break
		}
	}
	return out
}

func tagsCopy(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	return append([]string(nil), tags...)
}

func observationType(value, fallback string) string {
	switch value {
	case obsTypeSpan, obsTypeGeneration, obsTypeEvent, obsTypeEmbedding,
		obsTypeAgent, obsTypeTool, obsTypeChain, obsTypeRetriever,
		obsTypeGuardrail, obsTypeEvaluator:
		return value
	default:
		return fallback
	}
}
