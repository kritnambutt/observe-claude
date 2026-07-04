// Package tracer wires the real OpenTelemetry Go SDK to internal/store.
//
// A Claude Code session is observed across many independent, short-lived
// hook OS processes with no shared memory, but a span's lifecycle
// (Start/End) needs to happen inside a single process to go through the
// SDK's normal export path. We resolve this by only ever calling
// tracer.Start()+span.End() once *both* halves of an event pair are known
// (see internal/store's pending_spans table): the process handling the
// closing hook event creates the span and immediately ends it, using
// trace.WithTimestamp on both calls to backfill the real historical start
// and end times. The result is a genuine SDK-produced ReadOnlySpan exported
// through a real exporter, just constructed later than it "started".
//
// Trace/span IDs are not SDK-random: they're deterministic, derived from
// Claude's session_id and a per-session sequence number (internal/idgen),
// so independent processes agree on the same IDs without coordination.
package tracer

import (
	"context"
	crand "crypto/rand"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/papikayo/observability-code/internal/store"
)

type seedKey struct{}

type seed struct {
	traceID trace.TraceID
	spanID  trace.SpanID
}

// WithSeed forces the next span created from the returned context to use
// exactly traceID/spanID instead of an SDK-random pair.
func WithSeed(ctx context.Context, traceID trace.TraceID, spanID trace.SpanID) context.Context {
	return context.WithValue(ctx, seedKey{}, seed{traceID, spanID})
}

// WithParent marks ctx as having parentSpanID (in the same trace) as its
// parent span, so the next span created from it is linked as a child.
// Pass an empty parentSpanID for a root span (e.g. the session span).
func WithParent(ctx context.Context, traceID trace.TraceID, parentSpanID trace.SpanID) context.Context {
	if parentSpanID.IsValid() {
		sc := trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    traceID,
			SpanID:     parentSpanID,
			TraceFlags: trace.FlagsSampled,
			Remote:     true,
		})
		ctx = trace.ContextWithSpanContext(ctx, sc)
	}
	return ctx
}

// deterministicIDGenerator returns IDs seeded via WithSeed when present,
// falling back to crypto/rand so the provider still works for any span
// created without an explicit seed.
type deterministicIDGenerator struct{}

func (deterministicIDGenerator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	if s, ok := ctx.Value(seedKey{}).(seed); ok {
		return s.traceID, s.spanID
	}
	var tid trace.TraceID
	var sid trace.SpanID
	_, _ = crand.Read(tid[:])
	_, _ = crand.Read(sid[:])
	return tid, sid
}

func (deterministicIDGenerator) NewSpanID(ctx context.Context, _ trace.TraceID) trace.SpanID {
	if s, ok := ctx.Value(seedKey{}).(seed); ok {
		return s.spanID
	}
	var sid trace.SpanID
	_, _ = crand.Read(sid[:])
	return sid
}

// NewProvider builds a TracerProvider whose only job is to export finished
// spans into the local SQLite store for this one hook invocation.
// SimpleSpanProcessor is used deliberately over the batching processor:
// the process exits immediately after the hook does its work, so there is
// no window for a batch to flush in the background.
func NewProvider(st *store.Store, sessionID string) *sdktrace.TracerProvider {
	res := resource.NewSchemaless(
		attribute.String("service.name", "claude-code"),
		attribute.String("service.instance.id", sessionID),
	)
	return sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(NewExporter(st, sessionID))),
		sdktrace.WithIDGenerator(deterministicIDGenerator{}),
		sdktrace.WithResource(res),
	)
}

// Exporter writes finished spans directly into the SQLite store. It
// implements sdktrace.SpanExporter.
type Exporter struct {
	st        *store.Store
	sessionID string
}

func NewExporter(st *store.Store, sessionID string) *Exporter {
	return &Exporter{st: st, sessionID: sessionID}
}

// cco.kind carries our own span categorization (session/prompt/tool/agent/
// event) since it doesn't map cleanly onto OTel's SpanKind enum.
const KindAttrKey = "cco.kind"

func (e *Exporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, s := range spans {
		attrs := make(map[string]any, len(s.Attributes()))
		kind := s.SpanKind().String()
		for _, kv := range s.Attributes() {
			attrs[string(kv.Key)] = kv.Value.AsInterface()
			if string(kv.Key) == KindAttrKey {
				kind = kv.Value.AsString()
			}
		}

		status := "ok"
		if s.Status().Code == codes.Error {
			status = "error"
		}

		var parent string
		if s.Parent().SpanID().IsValid() {
			parent = s.Parent().SpanID().String()
		}

		fs := store.FinishedSpan{
			SpanID:       s.SpanContext().SpanID().String(),
			TraceID:      s.SpanContext().TraceID().String(),
			SessionID:    e.sessionID,
			ParentSpanID: parent,
			Name:         s.Name(),
			Kind:         kind,
			Status:       status,
			StartTime:    s.StartTime(),
			EndTime:      s.EndTime(),
			Attributes:   attrs,
		}
		if err := e.st.InsertSpan(fs); err != nil {
			return err
		}
	}
	return nil
}

func (e *Exporter) Shutdown(ctx context.Context) error { return nil }
