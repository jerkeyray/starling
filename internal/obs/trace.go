package obs

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// TracerName is the instrumentation scope name used across the
// library. Downstream exporters / processors can filter on it.
const TracerName = "github.com/jerkeyray/starling"

// Tracer returns the library's tracer. When no OTel SDK is configured
// the global provider is the no-op tracer, so call sites pay only the
// one-method-indirection price.
func Tracer() trace.Tracer {
	return otel.Tracer(TracerName)
}

// StartRunSpan opens the root span covering Agent.Run. Caller is
// responsible for calling End on the returned span (typically via
// defer). runID is bound as both a span attribute and a baggage-style
// annotation so log correlation is straightforward.
func StartRunSpan(ctx context.Context, runID string) (context.Context, trace.Span) {
	return Tracer().Start(ctx, "agent.run", trace.WithAttributes(
		attribute.String(AttrRunID, runID),
	))
}

// StartTurnSpan opens a child span covering one ReAct turn.
func StartTurnSpan(ctx context.Context, turnID string, turnNum int) (context.Context, trace.Span) {
	return Tracer().Start(ctx, "agent.turn", trace.WithAttributes(
		attribute.String(AttrTurnID, turnID),
		attribute.Int("turn", turnNum),
	))
}

// StartLLMSpan opens a child span covering a single step.LLMCall.
func StartLLMSpan(ctx context.Context, model string) (context.Context, trace.Span) {
	return Tracer().Start(ctx, "agent.llm_call", trace.WithAttributes(
		attribute.String("model", model),
	))
}

// StartToolSpan opens a child span covering one attempt of a tool
// call. Retries produce one span per attempt, each tagged with the
// attempt number so retry patterns are visible in the trace view.
func StartToolSpan(ctx context.Context, name, callID string, attempt int) (context.Context, trace.Span) {
	return Tracer().Start(ctx, "agent.tool_call", trace.WithAttributes(
		attribute.String(AttrToolName, name),
		attribute.String(AttrCallID, callID),
		attribute.Int(AttrAttempt, attempt),
	))
}

// SetSpanError marks the span as errored with the given error and a
// short status description. Used at every boundary that returns an
// error so the trace view is accurate.
func SetSpanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
