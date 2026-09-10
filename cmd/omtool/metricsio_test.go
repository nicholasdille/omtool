package main

import (
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
)

func TestNormalizeFormatAlias(t *testing.T) {
	cases := map[string]string{
		"om":          string(inOpenMetrics),
		"pb":          string(inProtobuf),
		"auto":        "auto",
		"openmetrics": "openmetrics",
		"":            "",
	}
	for in, want := range cases {
		if got := normalizeFormatAlias(in); got != want {
			t.Errorf("normalizeFormatAlias(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want inputFormat
	}{
		{"empty", []byte(""), inOpenMetrics},
		{"text", []byte("# HELP foo bar\n# TYPE foo counter\nfoo_total 1\n"), inOpenMetrics},
		{"binary with control bytes", []byte{0x0a, 0x03, 'f', 'o', 'o', 0x00, 0x01}, inProtobuf},
		{"invalid utf8", []byte{0xff, 0xfe, 0xfd}, inProtobuf},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectFormat(tt.data); got != tt.want {
				t.Errorf("detectFormat(%q) = %v, want %v", tt.data, got, tt.want)
			}
		})
	}
}

func TestParseOpenMetricsCounter(t *testing.T) {
	input := `# HELP http_requests_total total requests
# TYPE http_requests_total counter
http_requests_total{method="get"} 10
http_requests_total{method="post"} 5
# EOF
`
	families, err := parseOpenMetrics([]byte(input))
	if err != nil {
		t.Fatalf("parseOpenMetrics: %v", err)
	}
	if len(families) != 1 {
		t.Fatalf("expected 1 family, got %d", len(families))
	}
	fam := families[0]
	if fam.GetName() != "http_requests_total" {
		t.Errorf("name = %q", fam.GetName())
	}
	if fam.GetType() != dto.MetricType_COUNTER {
		t.Errorf("type = %v", fam.GetType())
	}
	if fam.GetHelp() != "total requests" {
		t.Errorf("help = %q", fam.GetHelp())
	}
	if len(fam.GetMetric()) != 2 {
		t.Fatalf("expected 2 metrics, got %d", len(fam.GetMetric()))
	}
	total := 0.0
	for _, m := range fam.GetMetric() {
		total += m.GetCounter().GetValue()
	}
	if total != 15 {
		t.Errorf("sum of counter values = %v, want 15", total)
	}
}

func TestParseOpenMetricsGauge(t *testing.T) {
	input := "# TYPE temp gauge\ntemp 23.5\n# EOF\n"
	families, err := parseOpenMetrics([]byte(input))
	if err != nil {
		t.Fatalf("parseOpenMetrics: %v", err)
	}
	if len(families) != 1 || families[0].GetType() != dto.MetricType_GAUGE {
		t.Fatalf("unexpected families: %+v", families)
	}
	if got := families[0].GetMetric()[0].GetGauge().GetValue(); got != 23.5 {
		t.Errorf("gauge value = %v, want 23.5", got)
	}
}

func TestParseOpenMetricsHistogram(t *testing.T) {
	input := `# TYPE latency histogram
latency_bucket{le="0.1"} 5
latency_bucket{le="0.5"} 8
latency_bucket{le="+Inf"} 10
latency_sum 3.2
latency_count 10
# EOF
`
	families, err := parseOpenMetrics([]byte(input))
	if err != nil {
		t.Fatalf("parseOpenMetrics: %v", err)
	}
	if len(families) != 1 {
		t.Fatalf("expected 1 family, got %d", len(families))
	}
	fam := families[0]
	if fam.GetType() != dto.MetricType_HISTOGRAM {
		t.Fatalf("type = %v", fam.GetType())
	}
	if len(fam.GetMetric()) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(fam.GetMetric()))
	}
	h := fam.GetMetric()[0].GetHistogram()
	if h.GetSampleCount() != 10 {
		t.Errorf("sample count = %d, want 10", h.GetSampleCount())
	}
	if h.GetSampleSum() != 3.2 {
		t.Errorf("sample sum = %v, want 3.2", h.GetSampleSum())
	}
	if len(h.GetBucket()) != 3 {
		t.Fatalf("expected 3 buckets, got %d", len(h.GetBucket()))
	}
}

func TestParseOpenMetricsSummary(t *testing.T) {
	input := `# TYPE latency summary
latency{quantile="0.5"} 1.0
latency{quantile="0.9"} 2.0
latency_sum 5.0
latency_count 3
# EOF
`
	families, err := parseOpenMetrics([]byte(input))
	if err != nil {
		t.Fatalf("parseOpenMetrics: %v", err)
	}
	s := families[0].GetMetric()[0].GetSummary()
	if s.GetSampleCount() != 3 || s.GetSampleSum() != 5.0 {
		t.Errorf("unexpected summary: %+v", s)
	}
	if len(s.GetQuantile()) != 2 {
		t.Fatalf("expected 2 quantiles, got %d", len(s.GetQuantile()))
	}
}

func TestParseOpenMetricsInvalid(t *testing.T) {
	if _, err := parseOpenMetrics([]byte("not valid openmetrics {{{")); err == nil {
		t.Error("expected error for invalid input")
	}
}

func TestGroupingKeyOrderIndependent(t *testing.T) {
	// Not directly exported as taking labels.Labels easily without the
	// prometheus label package here beyond what's already imported
	// elsewhere; instead validate indirectly through parseOpenMetrics that
	// series sharing a label set (regardless of label order) merge into a
	// single metric.
	input := `# TYPE h histogram
h_bucket{a="1",le="1"} 1
h_bucket{le="2",a="1"} 2
h_sum{a="1"} 1
h_count{a="1"} 2
# EOF
`
	families, err := parseOpenMetrics([]byte(input))
	if err != nil {
		t.Fatalf("parseOpenMetrics: %v", err)
	}
	if len(families[0].GetMetric()) != 1 {
		t.Fatalf("expected series to merge into 1 metric, got %d", len(families[0].GetMetric()))
	}
}

func TestDecodeProtobufRoundTrip(t *testing.T) {
	fam := &dto.MetricFamily{
		Name: proto.String("test_metric"),
		Type: dto.MetricType_GAUGE.Enum(),
		Metric: []*dto.Metric{
			{Gauge: &dto.Gauge{Value: proto.Float64(42)}},
		},
	}

	for _, format := range []pbFormat{pbDelimited, pbBinary, pbText, pbJSON} {
		t.Run(string(format), func(t *testing.T) {
			var buf strings.Builder
			if err := writeProtobuf(&buf, []*dto.MetricFamily{fam}, format); err != nil {
				t.Fatalf("writeProtobuf: %v", err)
			}
			got, err := decodeProtobuf([]byte(buf.String()), format)
			if err != nil {
				t.Fatalf("decodeProtobuf: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("expected 1 family, got %d", len(got))
			}
			if got[0].GetName() != "test_metric" {
				t.Errorf("name = %q", got[0].GetName())
			}
			if got[0].GetMetric()[0].GetGauge().GetValue() != 42 {
				t.Errorf("gauge value = %v, want 42", got[0].GetMetric()[0].GetGauge().GetValue())
			}
		})
	}
}

func TestDecodeProtobufUnknownFormat(t *testing.T) {
	if _, err := decodeProtobuf([]byte("x"), pbFormat("bogus")); err == nil {
		t.Error("expected error for unknown format")
	}
}

func TestMaybeDecompressSnappyPassthrough(t *testing.T) {
	data := []byte("plain data, not snappy framed")
	got, err := maybeDecompressSnappy(data)
	if err != nil {
		t.Fatalf("maybeDecompressSnappy: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("got %q, want passthrough of %q", got, data)
	}
}

func TestReadFamiliesAutoDetectsOpenMetrics(t *testing.T) {
	input := "# TYPE foo gauge\nfoo 1\n# EOF\n"
	families, err := readFamilies([]byte(input), inAuto, pbBinary)
	if err != nil {
		t.Fatalf("readFamilies: %v", err)
	}
	if len(families) != 1 || families[0].GetName() != "foo" {
		t.Fatalf("unexpected result: %+v", families)
	}
}

func TestReadFamiliesUnknownFormat(t *testing.T) {
	if _, err := readFamilies([]byte("x"), inputFormat("bogus"), pbBinary); err == nil {
		t.Error("expected error for unknown input format")
	}
}
