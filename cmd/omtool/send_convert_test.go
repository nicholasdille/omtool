package main

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/prometheus/prompb"
	"google.golang.org/protobuf/proto"
)

func findSeries(t *testing.T, wr *prompb.WriteRequest, name string, extra map[string]string) *prompb.TimeSeries {
	t.Helper()
	for i := range wr.Timeseries {
		ts := &wr.Timeseries[i]
		labelMap := map[string]string{}
		for _, l := range ts.Labels {
			labelMap[l.Name] = l.Value
		}
		if labelMap["__name__"] != name {
			continue
		}
		match := true
		for k, v := range extra {
			if labelMap[k] != v {
				match = false
				break
			}
		}
		if match {
			return ts
		}
	}
	return nil
}

func TestBuildWriteRequestCounter(t *testing.T) {
	families := []*dto.MetricFamily{
		{
			Name: proto.String("requests_total"),
			Type: dto.MetricType_COUNTER.Enum(),
			Help: proto.String("h"),
			Metric: []*dto.Metric{
				{
					Label:   []*dto.LabelPair{{Name: proto.String("method"), Value: proto.String("get")}},
					Counter: &dto.Counter{Value: proto.Float64(10)},
				},
			},
		},
	}
	wr := buildWriteRequest(families, 1000)

	if len(wr.Metadata) != 1 || wr.Metadata[0].Type != prompb.MetricMetadata_COUNTER {
		t.Fatalf("unexpected metadata: %+v", wr.Metadata)
	}
	ts := findSeries(t, wr, "requests_total", map[string]string{"method": "get"})
	if ts == nil {
		t.Fatal("expected series requests_total{method=\"get\"}")
	}
	if len(ts.Samples) != 1 || ts.Samples[0].Value != 10 || ts.Samples[0].Timestamp != 1000 {
		t.Errorf("unexpected samples: %+v", ts.Samples)
	}
}

func TestBuildWriteRequestUsesSampleTimestampWhenSet(t *testing.T) {
	families := []*dto.MetricFamily{
		{
			Name: proto.String("g"),
			Type: dto.MetricType_GAUGE.Enum(),
			Metric: []*dto.Metric{
				{Gauge: &dto.Gauge{Value: proto.Float64(1)}, TimestampMs: proto.Int64(555)},
			},
		},
	}
	wr := buildWriteRequest(families, 999)
	ts := findSeries(t, wr, "g", nil)
	if ts == nil {
		t.Fatal("expected series g")
	}
	if ts.Samples[0].Timestamp != 555 {
		t.Errorf("timestamp = %d, want 555 (explicit sample ts should win over default)", ts.Samples[0].Timestamp)
	}
}

func TestBuildWriteRequestSkipsUnnamedFamily(t *testing.T) {
	families := []*dto.MetricFamily{{Type: dto.MetricType_GAUGE.Enum()}}
	wr := buildWriteRequest(families, 0)
	if len(wr.Timeseries) != 0 || len(wr.Metadata) != 0 {
		t.Errorf("expected unnamed family to be skipped entirely, got %+v", wr)
	}
}

func TestBuildWriteRequestHistogram(t *testing.T) {
	families := []*dto.MetricFamily{
		{
			Name: proto.String("latency"),
			Type: dto.MetricType_HISTOGRAM.Enum(),
			Metric: []*dto.Metric{
				{
					Histogram: &dto.Histogram{
						SampleCount: proto.Uint64(10),
						SampleSum:   proto.Float64(3.5),
						Bucket: []*dto.Bucket{
							{UpperBound: proto.Float64(1), CumulativeCount: proto.Uint64(4)},
							{UpperBound: proto.Float64(2), CumulativeCount: proto.Uint64(10)},
						},
					},
				},
			},
		},
	}
	wr := buildWriteRequest(families, 100)

	if ts := findSeries(t, wr, "latency_bucket", map[string]string{"le": "1"}); ts == nil || ts.Samples[0].Value != 4 {
		t.Errorf("bucket le=1 missing or wrong: %+v", ts)
	}
	if ts := findSeries(t, wr, "latency_bucket", map[string]string{"le": "2"}); ts == nil || ts.Samples[0].Value != 10 {
		t.Errorf("bucket le=2 missing or wrong: %+v", ts)
	}
	if ts := findSeries(t, wr, "latency_sum", nil); ts == nil || ts.Samples[0].Value != 3.5 {
		t.Errorf("latency_sum missing or wrong: %+v", ts)
	}
	if ts := findSeries(t, wr, "latency_count", nil); ts == nil || ts.Samples[0].Value != 10 {
		t.Errorf("latency_count missing or wrong: %+v", ts)
	}
}

func TestBuildWriteRequestSummary(t *testing.T) {
	families := []*dto.MetricFamily{
		{
			Name: proto.String("latency"),
			Type: dto.MetricType_SUMMARY.Enum(),
			Metric: []*dto.Metric{
				{
					Summary: &dto.Summary{
						SampleCount: proto.Uint64(3),
						SampleSum:   proto.Float64(5),
						Quantile: []*dto.Quantile{
							{Quantile: proto.Float64(0.5), Value: proto.Float64(1)},
							{Quantile: proto.Float64(0.9), Value: proto.Float64(2)},
						},
					},
				},
			},
		},
	}
	wr := buildWriteRequest(families, 0)

	if ts := findSeries(t, wr, "latency", map[string]string{"quantile": "0.5"}); ts == nil || ts.Samples[0].Value != 1 {
		t.Errorf("quantile 0.5 missing or wrong: %+v", ts)
	}
	if ts := findSeries(t, wr, "latency_sum", nil); ts == nil || ts.Samples[0].Value != 5 {
		t.Errorf("latency_sum missing or wrong: %+v", ts)
	}
	if ts := findSeries(t, wr, "latency_count", nil); ts == nil || ts.Samples[0].Value != 3 {
		t.Errorf("latency_count missing or wrong: %+v", ts)
	}
}

func TestAppendSeriesLabelsSortedByName(t *testing.T) {
	wr := &prompb.WriteRequest{}
	appendSeries(wr, "foo", []prompb.Label{{Name: "z", Value: "1"}, {Name: "a", Value: "2"}}, 1, 0)
	if len(wr.Timeseries) != 1 {
		t.Fatalf("expected 1 series, got %d", len(wr.Timeseries))
	}
	labels := wr.Timeseries[0].Labels
	for i := 1; i < len(labels); i++ {
		if labels[i-1].Name > labels[i].Name {
			t.Errorf("labels not sorted: %+v", labels)
		}
	}
}

func TestWithExtraLabelDoesNotMutateBase(t *testing.T) {
	base := []prompb.Label{{Name: "a", Value: "1"}}
	extended := withExtraLabel(base, "le", "5")
	if len(base) != 1 {
		t.Errorf("base was mutated: %+v", base)
	}
	if len(extended) != 2 {
		t.Errorf("expected 2 labels in extended, got %d", len(extended))
	}
}

func TestToMetadataType(t *testing.T) {
	cases := map[dto.MetricType]prompb.MetricMetadata_MetricType{
		dto.MetricType_COUNTER:         prompb.MetricMetadata_COUNTER,
		dto.MetricType_GAUGE:           prompb.MetricMetadata_GAUGE,
		dto.MetricType_SUMMARY:         prompb.MetricMetadata_SUMMARY,
		dto.MetricType_HISTOGRAM:       prompb.MetricMetadata_HISTOGRAM,
		dto.MetricType_GAUGE_HISTOGRAM: prompb.MetricMetadata_GAUGEHISTOGRAM,
		dto.MetricType_UNTYPED:         prompb.MetricMetadata_UNKNOWN,
	}
	for in, want := range cases {
		if got := toMetadataType(in); got != want {
			t.Errorf("toMetadataType(%v) = %v, want %v", in, got, want)
		}
	}
}
