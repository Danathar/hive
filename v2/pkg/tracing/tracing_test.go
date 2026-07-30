package tracing

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestInit_DisabledIsNoop verifies that the default (disabled) path returns a
// usable no-op shutdown, records nothing, and never errors.
func TestInit_DisabledIsNoop(t *testing.T) {
	ctx := context.Background()

	shutdown, err := Init(ctx, Config{Enabled: false})
	if err != nil {
		t.Fatalf("Init(disabled) returned error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init(disabled) returned nil shutdown")
	}

	// StartSpan must work and not panic when tracing is disabled.
	_, span := StartSpan(ctx, "should.be.noop", attribute.String("k", "v"))
	if span.IsRecording() {
		t.Error("span should not be recording when tracing is disabled")
	}
	span.End()

	// Shutdown must be safe to call.
	if err := shutdown(ctx); err != nil {
		t.Errorf("no-op shutdown returned error: %v", err)
	}
}

// TestInit_EnabledMissingEndpointIsNoop verifies fail-closed behavior: enabling
// tracing with neither a configured endpoint nor the env var yields a no-op
// rather than defaulting to localhost and spewing connection errors.
func TestInit_EnabledMissingEndpointIsNoop(t *testing.T) {
	// Ensure the env var is unset for this test.
	t.Setenv(otelEndpointEnv, "")

	shutdown, err := Init(context.Background(), Config{Enabled: true})
	if err != nil {
		t.Fatalf("Init(enabled, no endpoint) returned error: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown returned error: %v", err)
	}
}

// TestStartSpan_RecordsWithStubExporter installs an in-memory SpanRecorder as
// the global provider and verifies StartSpan produces a span with the expected
// name and attributes. No real network is used.
func TestStartSpan_RecordsWithStubExporter(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(otel.GetTracerProvider())
		_ = tp.Shutdown(context.Background())
	})

	const spanName = "test.span"
	_, span := StartSpan(context.Background(), spanName,
		attribute.String("hive.id", "hive-123"),
		attribute.String("hive.branch", "v4"),
	)
	if !span.IsRecording() {
		t.Fatal("span should be recording with a real provider")
	}
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("expected 1 ended span, got %d", len(ended))
	}

	got := ended[0]
	if got.Name() != spanName {
		t.Errorf("span name = %q, want %q", got.Name(), spanName)
	}

	attrs := map[attribute.Key]string{}
	for _, kv := range got.Attributes() {
		attrs[kv.Key] = kv.Value.AsString()
	}
	if attrs["hive.id"] != "hive-123" {
		t.Errorf("hive.id attr = %q, want %q", attrs["hive.id"], "hive-123")
	}
	if attrs["hive.branch"] != "v4" {
		t.Errorf("hive.branch attr = %q, want %q", attrs["hive.branch"], "v4")
	}
}

// TestStartSpan_ParentChild verifies that a child span started from a parent's
// context is linked to the parent trace (the lifecycle shape we instrument).
func TestStartSpan_ParentChild(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx, parent := StartSpan(context.Background(), "governor.eval_cycle")
	childCtx, child := StartSpan(ctx, "governor.enumerate_actionable")
	_ = childCtx
	child.End()
	parent.End()

	ended := recorder.Ended()
	if len(ended) != 2 {
		t.Fatalf("expected 2 ended spans, got %d", len(ended))
	}

	// Find child and parent by name and confirm the child's parent is the
	// parent's span, sharing one trace ID.
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range ended {
		byName[s.Name()] = s
	}
	p := byName["governor.eval_cycle"]
	c := byName["governor.enumerate_actionable"]
	if p == nil || c == nil {
		t.Fatal("missing expected spans")
	}
	if c.Parent().SpanID() != p.SpanContext().SpanID() {
		t.Error("child span is not parented to the eval-cycle span")
	}
	if c.SpanContext().TraceID() != p.SpanContext().TraceID() {
		t.Error("child and parent spans have different trace IDs")
	}
}

// TestTracer_EmptyNameUsesDefault ensures Tracer("") does not panic and returns
// a usable tracer.
func TestTracer_EmptyNameUsesDefault(t *testing.T) {
	tr := Tracer("")
	if tr == nil {
		t.Fatal("Tracer(\"\") returned nil")
	}
	_, span := tr.Start(context.Background(), "x")
	span.End()
}
