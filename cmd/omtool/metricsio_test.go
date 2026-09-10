package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/golang/snappy"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
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
		{"DEL control byte", []byte{'f', 'o', 'o', 0x7f}, inProtobuf},
		{"truncated sample beyond 4096 bytes stays openmetrics", append([]byte("# TYPE foo gauge\n"), bytes.Repeat([]byte("foo 1\n"), 1000)...), inOpenMetrics},
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

func TestParseOpenMetricsCreatedTimestamps(t *testing.T) {
	input := `# TYPE requests counter
requests_total 10
requests_created 1000.5
# TYPE latency summary
latency{quantile="0.5"} 1
latency_sum 5
latency_count 3
latency_created 1000.25
# TYPE dur histogram
dur_bucket{le="+Inf"} 1
dur_sum 1
dur_count 1
dur_created 1000.75
# EOF
`
	families, err := parseOpenMetrics([]byte(input))
	if err != nil {
		t.Fatalf("parseOpenMetrics: %v", err)
	}
	byName := map[string]*dto.MetricFamily{}
	for _, f := range families {
		byName[f.GetName()] = f
	}

	c := byName["requests"].GetMetric()[0].GetCounter()
	if c.CreatedTimestamp == nil || c.GetCreatedTimestamp().AsTime().Unix() != 1000 {
		t.Errorf("counter created timestamp = %v", c.CreatedTimestamp)
	}

	s := byName["latency"].GetMetric()[0].GetSummary()
	if s.CreatedTimestamp == nil || s.GetCreatedTimestamp().AsTime().Unix() != 1000 {
		t.Errorf("summary created timestamp = %v", s.CreatedTimestamp)
	}

	h := byName["dur"].GetMetric()[0].GetHistogram()
	if h.CreatedTimestamp == nil || h.GetCreatedTimestamp().AsTime().Unix() != 1000 {
		t.Errorf("histogram created timestamp = %v", h.CreatedTimestamp)
	}
}

func TestParseOpenMetricsUnknownType(t *testing.T) {
	input := "# TYPE mystat unknown\nmystat 42\n# EOF\n"
	families, err := parseOpenMetrics([]byte(input))
	if err != nil {
		t.Fatalf("parseOpenMetrics: %v", err)
	}
	if len(families) != 1 || families[0].GetType() != dto.MetricType_UNTYPED {
		t.Fatalf("unexpected families: %+v", families)
	}
	if got := families[0].GetMetric()[0].GetUntyped().GetValue(); got != 42 {
		t.Errorf("untyped value = %v, want 42", got)
	}
}

func TestAddSeriesWithoutMetricName(t *testing.T) {
	groups := map[string]*metricGroup{}
	getGroup := func(name string) *metricGroup {
		g, ok := groups[name]
		if !ok {
			g = &metricGroup{family: &dto.MetricFamily{Name: proto.String(name), Type: dto.MetricType_GAUGE.Enum()}, metricsByID: map[string]*dto.Metric{}}
			groups[name] = g
		}
		return g
	}
	lbls := labels.FromStrings("job", "x")
	if err := addSeries(groups, getGroup, lbls, 1, nil); err == nil {
		t.Error("expected error for series without a metric name")
	}
}

func TestAddSeriesInvalidQuantileLabel(t *testing.T) {
	groups := map[string]*metricGroup{
		"latency": {family: &dto.MetricFamily{Name: proto.String("latency"), Type: dto.MetricType_SUMMARY.Enum()}, metricsByID: map[string]*dto.Metric{}},
	}
	getGroup := func(name string) *metricGroup { return groups[name] }
	lbls := labels.FromStrings(model.MetricNameLabel, "latency", "quantile", "not-a-number")
	if err := addSeries(groups, getGroup, lbls, 1, nil); err == nil {
		t.Error("expected error for invalid quantile label")
	}
}

func TestAddSeriesInvalidLeLabel(t *testing.T) {
	groups := map[string]*metricGroup{
		"dur": {family: &dto.MetricFamily{Name: proto.String("dur"), Type: dto.MetricType_HISTOGRAM.Enum()}, metricsByID: map[string]*dto.Metric{}},
	}
	getGroup := func(name string) *metricGroup { return groups[name] }
	lbls := labels.FromStrings(model.MetricNameLabel, "dur_bucket", "le", "not-a-number")
	if err := addSeries(groups, getGroup, lbls, 1, nil); err == nil {
		t.Error("expected error for invalid le label")
	}
}

func TestParseOpenMetricsSeriesTimestamps(t *testing.T) {
	input := `# TYPE h histogram
h_bucket{le="+Inf"} 1 100
h_sum 1 200
h_count 1 300
# EOF
`
	families, err := parseOpenMetrics([]byte(input))
	if err != nil {
		t.Fatalf("parseOpenMetrics: %v", err)
	}
	if len(families) != 1 || len(families[0].GetMetric()) != 1 {
		t.Fatalf("unexpected families: %+v", families)
	}
	m := families[0].GetMetric()[0]
	// The last series parsed for this metric (h_count) carries the final
	// timestamp, since each subsequent series sharing the same label set
	// overwrites TimestampMs.
	if m.GetTimestampMs() != 300000 {
		t.Errorf("timestamp = %d, want 300000", m.GetTimestampMs())
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

func TestMaybeDecompressSnappyRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	sw := snappy.NewBufferedWriter(&buf)
	if _, err := sw.Write([]byte("hello snappy world")); err != nil {
		t.Fatalf("writing snappy stream: %v", err)
	}
	if err := sw.Close(); err != nil {
		t.Fatalf("closing snappy writer: %v", err)
	}
	got, err := maybeDecompressSnappy(buf.Bytes())
	if err != nil {
		t.Fatalf("maybeDecompressSnappy: %v", err)
	}
	if string(got) != "hello snappy world" {
		t.Errorf("got %q, want %q", got, "hello snappy world")
	}
}

func TestMaybeDecompressSnappyCorrupt(t *testing.T) {
	corrupt := append([]byte{}, snappyStreamMagic...)
	corrupt = append(corrupt, []byte{0x01, 0x02, 0x03, 0x04}...)
	if _, err := maybeDecompressSnappy(corrupt); err == nil {
		t.Error("expected error for corrupt snappy stream")
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

func TestToProtoType(t *testing.T) {
	cases := []struct {
		in   model.MetricType
		want dto.MetricType
	}{
		{model.MetricTypeCounter, dto.MetricType_COUNTER},
		{model.MetricTypeGauge, dto.MetricType_GAUGE},
		{model.MetricTypeSummary, dto.MetricType_SUMMARY},
		{model.MetricTypeHistogram, dto.MetricType_HISTOGRAM},
		{model.MetricTypeGaugeHistogram, dto.MetricType_GAUGE_HISTOGRAM},
		{model.MetricTypeUnknown, dto.MetricType_UNTYPED},
	}
	for _, c := range cases {
		if got := toProtoType(c.in); got != c.want {
			t.Errorf("toProtoType(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestReadFamiliesUnknownFormat(t *testing.T) {
	if _, err := readFamilies([]byte("x"), inputFormat("bogus"), pbBinary); err == nil {
		t.Error("expected error for unknown input format")
	}
}

func TestReadFamiliesOpenMetricsError(t *testing.T) {
	if _, err := readFamilies([]byte("not valid openmetrics {{{"), inOpenMetrics, pbBinary); err == nil {
		t.Error("expected error for invalid OpenMetrics input")
	}
}

func TestReadFamiliesProtobufError(t *testing.T) {
	if _, err := readFamilies([]byte("x"), inProtobuf, pbFormat("bogus")); err == nil {
		t.Error("expected error for unknown protobuf format")
	}
}

func TestDecodeBinaryEmpty(t *testing.T) {
	got, err := decodeBinary([]byte("   \n"))
	if err != nil {
		t.Fatalf("decodeBinary: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil families for empty input, got %+v", got)
	}
}

func TestDecodeBinaryInvalid(t *testing.T) {
	if _, err := decodeBinary([]byte{0xff, 0xff, 0xff}); err == nil {
		t.Error("expected error for invalid binary protobuf")
	}
}

func TestDecodeBinaryRoundTrip(t *testing.T) {
	mf := &dto.MetricFamily{Name: proto.String("foo"), Type: dto.MetricType_GAUGE.Enum()}
	b, err := proto.Marshal(mf)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	got, err := decodeBinary(b)
	if err != nil {
		t.Fatalf("decodeBinary: %v", err)
	}
	if len(got) != 1 || got[0].GetName() != "foo" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestDecodeTextInvalid(t *testing.T) {
	if _, err := decodeText([]byte("not valid prototext {{{")); err == nil {
		t.Error("expected error for invalid prototext")
	}
}

func TestDecodeJSONInvalid(t *testing.T) {
	if _, err := decodeJSON([]byte("not valid json")); err == nil {
		t.Error("expected error for invalid protojson")
	}
}

func TestDecodeJSONRoundTrip(t *testing.T) {
	got, err := decodeJSON([]byte(`{"name":"foo","type":"GAUGE"}` + "\n"))
	if err != nil {
		t.Fatalf("decodeJSON: %v", err)
	}
	if len(got) != 1 || got[0].GetName() != "foo" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestDecodeDelimitedInvalid(t *testing.T) {
	if _, err := decodeDelimited([]byte{0xff, 0xff, 0xff, 0xff, 0xff}); err == nil {
		t.Error("expected error for invalid delimited protobuf")
	}
}
