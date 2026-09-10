package main

// This file implements the "convert" subcommand: converting Prometheus/
// OpenMetrics metric data between OpenMetrics text exposition format,
// Prometheus protobuf MetricFamily messages, and human readable text.

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang/snappy"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
)

// outputFormat identifies what kind of output omtool produces.
type outputFormat string

const (
	outOpenMetrics outputFormat = "openmetrics" // OpenMetrics text exposition format
	outProtobuf    outputFormat = "protobuf"    // protobuf MetricFamily messages, encoded per -pb-format
	outHuman       outputFormat = "human"       // human readable, indented plain text
)

// newConvertCmd builds the "convert" subcommand: it converts metric data
// between OpenMetrics, protobuf and human readable formats.
func newConvertCmd() *cobra.Command {
	var (
		inPath, outPath   string
		from, to, pbFmtFl string
		snappyFl          bool
	)

	cmd := &cobra.Command{
		Use:   "convert",
		Short: "Convert metric data between OpenMetrics, protobuf and human readable formats",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := run(inPath, outPath, inputFormat(normalizeFormatAlias(from)), outputFormat(normalizeFormatAlias(to)), pbFormat(pbFmtFl), snappyFl); err != nil {
				return fmt.Errorf("omtool convert: %w", err)
			}
			return nil
		},
	}

	fs := cmd.Flags()
	fs.StringVar(&inPath, "in", "", `input file ("-" for stdin)`)
	fs.StringVar(&outPath, "out", "-", `output file ("-" for stdout)`)
	fs.StringVar(&from, "from", string(inAuto), "input format: auto|openmetrics(om)|protobuf(pb)")
	fs.StringVar(&to, "to", string(outHuman), "output format: openmetrics(om)|protobuf(pb)|human")
	fs.StringVar(&pbFmtFl, "pb-format", string(pbBinary), "protobuf framing, for --from=protobuf and/or --to=protobuf: delimited|binary|text|json")
	fs.BoolVar(&snappyFl, "snappy", false, "compress protobuf output with Snappy (only applies to --to=protobuf)")
	_ = cmd.MarkFlagRequired("in")

	return cmd
}

func run(inPath, outPath string, from inputFormat, to outputFormat, pbFmt pbFormat, snappyCompress bool) error {
	families, err := loadFamilies(inPath, from, pbFmt)
	if err != nil {
		return err
	}

	out, err := openOutput(outPath)
	if err != nil {
		return fmt.Errorf("opening output: %w", err)
	}
	defer func() { _ = out.Close() }()

	w := bufio.NewWriter(out)
	switch to {
	case outOpenMetrics:
		err = writeOpenMetrics(w, families)
	case outProtobuf:
		if snappyCompress {
			sw := snappy.NewBufferedWriter(w)
			if err = writeProtobuf(sw, families, pbFmt); err == nil {
				err = sw.Close()
			}
		} else {
			err = writeProtobuf(w, families, pbFmt)
		}
	case outHuman:
		err = renderFamilies(w, families)
	default:
		err = fmt.Errorf("unknown output format %q (want openmetrics(om)|protobuf(pb)|human)", to)
	}
	if err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return w.Flush()
}

// writeProtobuf encodes families as protobuf MetricFamily messages using
// the requested framing.
func writeProtobuf(w io.Writer, families []*dto.MetricFamily, format pbFormat) error {
	switch format {
	case pbDelimited:
		enc := expfmt.NewEncoder(w, expfmt.NewFormat(expfmt.TypeProtoDelim))
		for _, mf := range families {
			if err := enc.Encode(mf); err != nil {
				return err
			}
		}
		return nil

	case pbBinary:
		for _, mf := range families {
			b, err := proto.Marshal(mf)
			if err != nil {
				return err
			}
			if _, err := w.Write(b); err != nil {
				return err
			}
		}
		return nil

	case pbText:
		for _, mf := range families {
			b, err := prototext.MarshalOptions{Multiline: true}.Marshal(mf)
			if err != nil {
				return err
			}
			if _, err := w.Write(b); err != nil {
				return err
			}
			if _, err := w.Write([]byte("\n")); err != nil {
				return err
			}
		}
		return nil

	case pbJSON:
		for _, mf := range families {
			b, err := protojson.MarshalOptions{Multiline: false}.Marshal(mf)
			if err != nil {
				return err
			}
			if _, err := w.Write(b); err != nil {
				return err
			}
			if _, err := w.Write([]byte("\n")); err != nil {
				return err
			}
		}
		return nil

	default:
		return fmt.Errorf("unknown protobuf format %q (want delimited|binary|text|json)", format)
	}
}

// writeOpenMetrics renders families as OpenMetrics text exposition format.
func writeOpenMetrics(w io.Writer, families []*dto.MetricFamily) error {
	for _, mf := range families {
		if _, err := expfmt.MetricFamilyToOpenMetrics(w, mf); err != nil {
			return err
		}
	}
	_, err := expfmt.FinalizeOpenMetrics(w)
	return err
}

// ---- human readable rendering ----

// renderFamilies writes a human readable rendition of each MetricFamily:
// its name, type and help text, followed by every metric with its labels
// and value(s) spelled out (including histogram buckets and summary
// quantiles).
func renderFamilies(w io.Writer, families []*dto.MetricFamily) error {
	for i, mf := range families {
		if i > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		if err := renderFamily(w, mf); err != nil {
			return err
		}
	}
	return nil
}

func renderFamily(w io.Writer, mf *dto.MetricFamily) error {
	name := mf.GetName()
	if name == "" {
		name = "(unnamed)"
	}
	if _, err := fmt.Fprintf(w, "Metric family: %s\n", name); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  Type: %s\n", mf.GetType()); err != nil {
		return err
	}
	if help := mf.GetHelp(); help != "" {
		if _, err := fmt.Fprintf(w, "  Help: %s\n", help); err != nil {
			return err
		}
	}
	if len(mf.GetMetric()) == 0 {
		_, err := fmt.Fprintln(w, "  (no metrics)")
		return err
	}
	if _, err := fmt.Fprintln(w, "  Metrics:"); err != nil {
		return err
	}
	for _, m := range mf.GetMetric() {
		if err := renderMetric(w, mf.GetType(), m); err != nil {
			return err
		}
	}
	return nil
}

func renderMetric(w io.Writer, mtype dto.MetricType, m *dto.Metric) error {
	if _, err := fmt.Fprintf(w, "    - labels: %s\n", formatLabels(m.GetLabel())); err != nil {
		return err
	}
	if ts := m.GetTimestampMs(); ts != 0 {
		t := time.UnixMilli(ts).UTC()
		if _, err := fmt.Fprintf(w, "      timestamp: %s\n", t.Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}

	switch mtype {
	case dto.MetricType_COUNTER:
		c := m.GetCounter()
		if _, err := fmt.Fprintf(w, "      value: %s\n", formatFloat(c.GetValue())); err != nil {
			return err
		}
		if c.CreatedTimestamp != nil {
			if _, err := fmt.Fprintf(w, "      created: %s\n", c.GetCreatedTimestamp().AsTime().UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}

	case dto.MetricType_GAUGE:
		if _, err := fmt.Fprintf(w, "      value: %s\n", formatFloat(m.GetGauge().GetValue())); err != nil {
			return err
		}

	case dto.MetricType_SUMMARY:
		s := m.GetSummary()
		if _, err := fmt.Fprintf(w, "      sample count: %d\n", s.GetSampleCount()); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      sample sum: %s\n", formatFloat(s.GetSampleSum())); err != nil {
			return err
		}
		quantiles := append([]*dto.Quantile(nil), s.GetQuantile()...)
		sort.Slice(quantiles, func(i, j int) bool { return quantiles[i].GetQuantile() < quantiles[j].GetQuantile() })
		for _, q := range quantiles {
			if _, err := fmt.Fprintf(w, "      quantile %s: %s\n", formatFloat(q.GetQuantile()), formatFloat(q.GetValue())); err != nil {
				return err
			}
		}
		if s.CreatedTimestamp != nil {
			if _, err := fmt.Fprintf(w, "      created: %s\n", s.GetCreatedTimestamp().AsTime().UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}

	case dto.MetricType_HISTOGRAM, dto.MetricType_GAUGE_HISTOGRAM:
		h := m.GetHistogram()
		if _, err := fmt.Fprintf(w, "      sample count: %d\n", h.GetSampleCount()); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      sample sum: %s\n", formatFloat(h.GetSampleSum())); err != nil {
			return err
		}
		buckets := append([]*dto.Bucket(nil), h.GetBucket()...)
		sort.Slice(buckets, func(i, j int) bool { return buckets[i].GetUpperBound() < buckets[j].GetUpperBound() })
		for _, b := range buckets {
			if _, err := fmt.Fprintf(w, "      bucket le %s: %d\n", formatFloat(b.GetUpperBound()), b.GetCumulativeCount()); err != nil {
				return err
			}
		}
		if h.CreatedTimestamp != nil {
			if _, err := fmt.Fprintf(w, "      created: %s\n", h.GetCreatedTimestamp().AsTime().UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}

	default: // UNTYPED
		if _, err := fmt.Fprintf(w, "      value: %s\n", formatFloat(m.GetUntyped().GetValue())); err != nil {
			return err
		}
	}
	return nil
}

func formatLabels(labels []*dto.LabelPair) string {
	if len(labels) == 0 {
		return "{}"
	}
	sorted := append([]*dto.LabelPair(nil), labels...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].GetName() < sorted[j].GetName() })
	parts := make([]string, 0, len(sorted))
	for _, l := range sorted {
		parts = append(parts, fmt.Sprintf("%s=%q", l.GetName(), l.GetValue()))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// formatFloat renders f the way Prometheus text exposition does: plain
// decimal notation with no trailing zeros, and the special +Inf/-Inf/NaN
// tokens for non-finite values.
func formatFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	case math.IsNaN(f):
		return "NaN"
	default:
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
}
