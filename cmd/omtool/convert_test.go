package main

import (
	"bytes"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRenderFamiliesWriteErrors(t *testing.T) {
	created := timestamppb.New(time.Unix(1000, 0))
	families := []*dto.MetricFamily{
		{
			Name: proto.String("g"), Help: proto.String("help"), Type: dto.MetricType_GAUGE.Enum(),
			Metric: []*dto.Metric{{Gauge: &dto.Gauge{Value: proto.Float64(3.5)}, TimestampMs: proto.Int64(1000)}},
		},
		{
			Name: proto.String("c"), Type: dto.MetricType_COUNTER.Enum(),
			Metric: []*dto.Metric{{Counter: &dto.Counter{Value: proto.Float64(1), CreatedTimestamp: created}}},
		},
		{
			Name: proto.String("s"), Type: dto.MetricType_SUMMARY.Enum(),
			Metric: []*dto.Metric{{Summary: &dto.Summary{
				SampleCount:      proto.Uint64(2),
				SampleSum:        proto.Float64(4),
				Quantile:         []*dto.Quantile{{Quantile: proto.Float64(0.9), Value: proto.Float64(2)}, {Quantile: proto.Float64(0.5), Value: proto.Float64(1)}},
				CreatedTimestamp: created,
			}}},
		},
		{
			Name: proto.String("h"), Type: dto.MetricType_HISTOGRAM.Enum(),
			Metric: []*dto.Metric{{Histogram: &dto.Histogram{
				SampleCount: proto.Uint64(2),
				SampleSum:   proto.Float64(4),
				Bucket: []*dto.Bucket{
					{UpperBound: proto.Float64(1), CumulativeCount: proto.Uint64(1)},
					{UpperBound: proto.Float64(math.Inf(1)), CumulativeCount: proto.Uint64(2)},
				},
				CreatedTimestamp: created,
			}}},
		},
		{
			Name: proto.String("u"), Type: dto.MetricType_UNTYPED.Enum(),
			Metric: []*dto.Metric{{Untyped: &dto.Untyped{Value: proto.Float64(7)}}},
		},
		{Type: dto.MetricType_UNTYPED.Enum()}, // unnamed, no metrics
	}

	// Fail at every possible write count from 0 up to the point every
	// write succeeds, so every "if err != nil { return err }" guard
	// following a Fprint(f/ln) call gets exercised as the failing write
	// at least once.
	for n := 0; n < 60; n++ {
		if err := renderFamilies(&failAfterWriter{n: n}, families); err == nil {
			break
		}
	}
}

func TestWriteOpenMetricsWriteErrors(t *testing.T) {
	families := []*dto.MetricFamily{
		{Name: proto.String("foo"), Type: dto.MetricType_GAUGE.Enum(), Metric: []*dto.Metric{{Gauge: &dto.Gauge{Value: proto.Float64(1)}}}},
	}
	for n := 0; n < 10; n++ {
		if err := writeOpenMetrics(&failAfterWriter{n: n}, families); err == nil {
			break
		}
	}
}

func TestNewConvertCmdRunEError(t *testing.T) {
	cmd := newConvertCmd()
	cmd.SetArgs([]string{"--in", "/nonexistent/does-not-exist.om"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing input file")
	}
	if !strings.Contains(err.Error(), "omtool convert:") {
		t.Errorf("expected wrapped error, got: %v", err)
	}
}

func TestNewConvertCmdRunESuccess(t *testing.T) {
	dir := t.TempDir()
	inPath := dir + "/in.om"
	if err := writeTempFile(inPath, "# TYPE foo gauge\nfoo 1\n# EOF\n"); err != nil {
		t.Fatalf("writeTempFile: %v", err)
	}
	outPath := dir + "/out.txt"
	cmd := newConvertCmd()
	cmd.SetArgs([]string{"--in", inPath, "--out", outPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestFormatFloat(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{1.5, "1.5"},
		{math.Inf(1), "+Inf"},
		{math.Inf(-1), "-Inf"},
		{math.NaN(), "NaN"},
	}
	for _, c := range cases {
		if got := formatFloat(c.in); got != c.want {
			t.Errorf("formatFloat(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatLabels(t *testing.T) {
	if got := formatLabels(nil); got != "{}" {
		t.Errorf("formatLabels(nil) = %q, want {}", got)
	}

	labels := []*dto.LabelPair{
		{Name: proto.String("b"), Value: proto.String("2")},
		{Name: proto.String("a"), Value: proto.String("1")},
	}
	got := formatLabels(labels)
	want := `{a="1", b="2"}`
	if got != want {
		t.Errorf("formatLabels = %q, want %q (labels should be sorted by name)", got, want)
	}
}

func TestRenderFamiliesCounter(t *testing.T) {
	fam := &dto.MetricFamily{
		Name: proto.String("requests_total"),
		Help: proto.String("total requests"),
		Type: dto.MetricType_COUNTER.Enum(),
		Metric: []*dto.Metric{
			{
				Label:   []*dto.LabelPair{{Name: proto.String("method"), Value: proto.String("get")}},
				Counter: &dto.Counter{Value: proto.Float64(10)},
			},
		},
	}

	var buf bytes.Buffer
	if err := renderFamilies(&buf, []*dto.MetricFamily{fam}); err != nil {
		t.Fatalf("renderFamilies: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Metric family: requests_total", "Type: COUNTER", "Help: total requests", `labels: {method="get"}`, "value: 10"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
}

func TestRenderFamiliesUnnamed(t *testing.T) {
	fam := &dto.MetricFamily{Type: dto.MetricType_UNTYPED.Enum()}
	var buf bytes.Buffer
	if err := renderFamilies(&buf, []*dto.MetricFamily{fam}); err != nil {
		t.Fatalf("renderFamilies: %v", err)
	}
	if !strings.Contains(buf.String(), "(unnamed)") {
		t.Errorf("expected (unnamed) placeholder, got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "(no metrics)") {
		t.Errorf("expected (no metrics) placeholder, got:\n%s", buf.String())
	}
}

func TestRenderMetricAllTypes(t *testing.T) {
	created := timestamppb.New(time.Unix(1000, 0))
	families := []*dto.MetricFamily{
		{
			Name: proto.String("g"), Type: dto.MetricType_GAUGE.Enum(),
			Metric: []*dto.Metric{{Gauge: &dto.Gauge{Value: proto.Float64(3.5)}, TimestampMs: proto.Int64(1000)}},
		},
		{
			Name: proto.String("s"), Type: dto.MetricType_SUMMARY.Enum(),
			Metric: []*dto.Metric{{Summary: &dto.Summary{
				SampleCount:      proto.Uint64(2),
				SampleSum:        proto.Float64(4),
				Quantile:         []*dto.Quantile{{Quantile: proto.Float64(0.9), Value: proto.Float64(2)}, {Quantile: proto.Float64(0.5), Value: proto.Float64(1)}},
				CreatedTimestamp: created,
			}}},
		},
		{
			Name: proto.String("h"), Type: dto.MetricType_HISTOGRAM.Enum(),
			Metric: []*dto.Metric{{Histogram: &dto.Histogram{
				SampleCount: proto.Uint64(2),
				SampleSum:   proto.Float64(4),
				Bucket: []*dto.Bucket{
					{UpperBound: proto.Float64(math.Inf(1)), CumulativeCount: proto.Uint64(2)},
					{UpperBound: proto.Float64(1), CumulativeCount: proto.Uint64(1)},
				},
				CreatedTimestamp: created,
			}}},
		},
		{
			Name: proto.String("u"), Type: dto.MetricType_UNTYPED.Enum(),
			Metric: []*dto.Metric{{Untyped: &dto.Untyped{Value: proto.Float64(7)}}},
		},
		{
			Name: proto.String("c"), Type: dto.MetricType_COUNTER.Enum(),
			Metric: []*dto.Metric{{Counter: &dto.Counter{Value: proto.Float64(1), CreatedTimestamp: created}}},
		},
	}

	var buf bytes.Buffer
	if err := renderFamilies(&buf, families); err != nil {
		t.Fatalf("renderFamilies: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"value: 3.5", "timestamp: ",
		"sample count: 2", "sample sum: 4", "quantile 0.5: 1", "quantile 0.9: 2",
		"bucket le 1: 1", "bucket le +Inf: 2",
		"value: 7",
		"created: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
}

func TestWriteOpenMetrics(t *testing.T) {
	fam := &dto.MetricFamily{
		Name: proto.String("foo"),
		Type: dto.MetricType_GAUGE.Enum(),
		Metric: []*dto.Metric{
			{Gauge: &dto.Gauge{Value: proto.Float64(5)}},
		},
	}
	var buf bytes.Buffer
	if err := writeOpenMetrics(&buf, []*dto.MetricFamily{fam}); err != nil {
		t.Fatalf("writeOpenMetrics: %v", err)
	}
	if !strings.Contains(buf.String(), "foo 5") {
		t.Errorf("expected exposition of foo, got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "# EOF") {
		t.Errorf("expected OpenMetrics EOF marker, got:\n%s", buf.String())
	}
}

func TestWriteProtobufWriteErrors(t *testing.T) {
	families := []*dto.MetricFamily{
		{Name: proto.String("foo"), Type: dto.MetricType_GAUGE.Enum(), Metric: []*dto.Metric{{Gauge: &dto.Gauge{Value: proto.Float64(1)}}}},
	}
	for _, format := range []pbFormat{pbDelimited, pbBinary, pbText, pbJSON} {
		for n := 0; n < 10; n++ {
			if err := writeProtobuf(&failAfterWriter{n: n}, families, format); err == nil {
				break
			}
		}
	}
}

func TestWriteProtobufUnknownFormat(t *testing.T) {
	var buf bytes.Buffer
	if err := writeProtobuf(&buf, nil, pbFormat("bogus")); err == nil {
		t.Error("expected error for unknown protobuf format")
	}
}

func TestRunUnknownOutputFormat(t *testing.T) {
	dir := t.TempDir()
	inPath := dir + "/in.om"
	if err := writeTempFile(inPath, "# TYPE foo gauge\nfoo 1\n# EOF\n"); err != nil {
		t.Fatalf("writeTempFile: %v", err)
	}
	outPath := dir + "/out"
	if err := run(inPath, outPath, inOpenMetrics, outputFormat("bogus"), pbBinary, false); err == nil {
		t.Error("expected error for unknown output format")
	}
}

func TestRunOpenMetricsToHuman(t *testing.T) {
	dir := t.TempDir()
	inPath := dir + "/in.om"
	if err := writeTempFile(inPath, "# TYPE foo gauge\nfoo 7\n# EOF\n"); err != nil {
		t.Fatalf("writeTempFile: %v", err)
	}
	outPath := dir + "/out.txt"
	if err := run(inPath, outPath, inOpenMetrics, outHuman, pbBinary, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := readTempFile(outPath)
	if err != nil {
		t.Fatalf("readTempFile: %v", err)
	}
	if !strings.Contains(got, "Metric family: foo") || !strings.Contains(got, "value: 7") {
		t.Errorf("unexpected output:\n%s", got)
	}
}

func TestRunOpenMetricsToOpenMetrics(t *testing.T) {
	dir := t.TempDir()
	inPath := dir + "/in.om"
	if err := writeTempFile(inPath, "# TYPE foo gauge\nfoo 7\n# EOF\n"); err != nil {
		t.Fatalf("writeTempFile: %v", err)
	}
	outPath := dir + "/out.om"
	if err := run(inPath, outPath, inOpenMetrics, outOpenMetrics, pbBinary, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := readTempFile(outPath)
	if err != nil {
		t.Fatalf("readTempFile: %v", err)
	}
	if !strings.Contains(got, "foo") || !strings.Contains(got, "# EOF") {
		t.Errorf("unexpected output:\n%s", got)
	}
}

func TestRunToProtobufFormats(t *testing.T) {
	dir := t.TempDir()
	inPath := dir + "/in.om"
	if err := writeTempFile(inPath, "# TYPE foo gauge\nfoo 7\n# EOF\n"); err != nil {
		t.Fatalf("writeTempFile: %v", err)
	}
	for _, format := range []pbFormat{pbDelimited, pbBinary, pbText, pbJSON} {
		for _, snappyCompress := range []bool{false, true} {
			outPath := dir + "/out-" + string(format) + ".pb"
			if err := run(inPath, outPath, inOpenMetrics, outProtobuf, format, snappyCompress); err != nil {
				t.Fatalf("run(format=%s, snappy=%v): %v", format, snappyCompress, err)
			}
		}
	}
}

func TestRunLoadFamiliesError(t *testing.T) {
	dir := t.TempDir()
	if err := run(dir+"/does-not-exist.om", dir+"/out", inOpenMetrics, outHuman, pbBinary, false); err == nil {
		t.Error("expected error for missing input file")
	}
}

func TestRunOpenOutputError(t *testing.T) {
	dir := t.TempDir()
	inPath := dir + "/in.om"
	if err := writeTempFile(inPath, "# TYPE foo gauge\nfoo 1\n# EOF\n"); err != nil {
		t.Fatalf("writeTempFile: %v", err)
	}
	if err := run(inPath, dir+"/no-such-dir/out.txt", inOpenMetrics, outHuman, pbBinary, false); err == nil {
		t.Error("expected error opening output in nonexistent directory")
	}
}
