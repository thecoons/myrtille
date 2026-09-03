package oteltrace

import (
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func strAttr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   key,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}},
	}
}

func span(name string, startNano, endNano uint64, code tracepb.Status_StatusCode) *tracepb.Span {
	return &tracepb.Span{
		Name:              name,
		StartTimeUnixNano: startNano,
		EndTimeUnixNano:   endNano,
		Status:            &tracepb.Status{Code: code},
	}
}

func TestReduceSpansComputesDurationInMilliseconds(t *testing.T) {
	start := uint64(1_000_000_000) // arbitrary non-zero start, in ns
	end := start + uint64(42*time.Millisecond)

	req := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{
			{
				ScopeSpans: []*tracepb.ScopeSpans{
					{Spans: []*tracepb.Span{
						span("check_inventory", start, end, tracepb.Status_STATUS_CODE_OK),
					}},
				},
			},
		},
	}

	got := reduceSpans(req)
	if len(got) != 1 {
		t.Fatalf("expected 1 span sample, got %d", len(got))
	}
	if got[0].durationMs != 42 {
		t.Errorf("expected duration 42ms, got %v", got[0].durationMs)
	}
}

func TestReduceSpansStatusOKAndError(t *testing.T) {
	req := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{
			{
				ScopeSpans: []*tracepb.ScopeSpans{
					{Spans: []*tracepb.Span{
						span("ok_span", 0, uint64(10*time.Millisecond), tracepb.Status_STATUS_CODE_OK),
						span("err_span", 0, uint64(10*time.Millisecond), tracepb.Status_STATUS_CODE_ERROR),
						span("unset_span", 0, uint64(10*time.Millisecond), tracepb.Status_STATUS_CODE_UNSET),
					}},
				},
			},
		},
	}

	got := reduceSpans(req)
	if len(got) != 3 {
		t.Fatalf("expected 3 span samples, got %d", len(got))
	}

	byName := make(map[string]spanSample, 3)
	for _, s := range got {
		byName[s.name] = s
	}

	if byName["ok_span"].isError {
		t.Error("expected ok_span.isError == false")
	}
	if !byName["err_span"].isError {
		t.Error("expected err_span.isError == true")
	}
	if byName["unset_span"].isError {
		t.Error("expected unset_span.isError == false (UNSET is not ERROR)")
	}
}

func TestReduceSpansMultipleSpansAndResourceGroupsInOneBatch(t *testing.T) {
	req := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{
			{
				Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{strAttr("service.name", "orders-api")}},
				ScopeSpans: []*tracepb.ScopeSpans{
					{Spans: []*tracepb.Span{
						span("insert_order", 0, uint64(5*time.Millisecond), tracepb.Status_STATUS_CODE_OK),
						span("check_inventory", 0, uint64(3*time.Millisecond), tracepb.Status_STATUS_CODE_OK),
					}},
				},
			},
			{
				Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{strAttr("service.name", "stock-service")}},
				ScopeSpans: []*tracepb.ScopeSpans{
					{Spans: []*tracepb.Span{
						span("reserve_stock", 0, uint64(2*time.Millisecond), tracepb.Status_STATUS_CODE_OK),
					}},
				},
			},
		},
	}

	got := reduceSpans(req)
	if len(got) != 3 {
		t.Fatalf("expected 3 span samples across both resource groups, got %d", len(got))
	}

	byName := make(map[string]spanSample, 3)
	for _, s := range got {
		byName[s.name] = s
	}

	if byName["insert_order"].otelSvc != "orders-api" {
		t.Errorf("expected insert_order.otelSvc = orders-api, got %q", byName["insert_order"].otelSvc)
	}
	if byName["check_inventory"].otelSvc != "orders-api" {
		t.Errorf("expected check_inventory.otelSvc = orders-api, got %q", byName["check_inventory"].otelSvc)
	}
	if byName["reserve_stock"].otelSvc != "stock-service" {
		t.Errorf("expected reserve_stock.otelSvc = stock-service, got %q", byName["reserve_stock"].otelSvc)
	}
}

func TestReduceSpansNoServiceNameAttributeYieldsEmptyOtelSvc(t *testing.T) {
	req := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{
			{
				Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{strAttr("other.attr", "x")}},
				ScopeSpans: []*tracepb.ScopeSpans{
					{Spans: []*tracepb.Span{span("noop", 0, uint64(time.Millisecond), tracepb.Status_STATUS_CODE_OK)}},
				},
			},
			{
				// No Resource at all.
				ScopeSpans: []*tracepb.ScopeSpans{
					{Spans: []*tracepb.Span{span("noop2", 0, uint64(time.Millisecond), tracepb.Status_STATUS_CODE_OK)}},
				},
			},
		},
	}

	got := reduceSpans(req)
	for _, s := range got {
		if s.otelSvc != "" {
			t.Errorf("expected empty otelSvc for %s, got %q", s.name, s.otelSvc)
		}
	}
}

func TestReduceSpansSkipsEndBeforeStart(t *testing.T) {
	req := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{
			{
				ScopeSpans: []*tracepb.ScopeSpans{
					{Spans: []*tracepb.Span{
						span("malformed", 100, 50, tracepb.Status_STATUS_CODE_OK),
						span("well_formed", 0, uint64(time.Millisecond), tracepb.Status_STATUS_CODE_OK),
					}},
				},
			},
		},
	}

	got := reduceSpans(req)
	if len(got) != 1 || got[0].name != "well_formed" {
		t.Fatalf("expected only well_formed to survive, got %+v", got)
	}
}

func TestResourceServiceNameHandlesNilResource(t *testing.T) {
	if got := resourceServiceName(nil); got != "" {
		t.Errorf("expected empty string for nil resource, got %q", got)
	}
}
