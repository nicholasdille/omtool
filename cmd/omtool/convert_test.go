package main

import (
	"bytes"
	"math"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
)

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
