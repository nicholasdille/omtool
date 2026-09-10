package main

// This file implements OpenMetrics/protobuf parsing and framing shared by
// omtool's "convert" and "send" subcommands, so both accept the same input
// formats.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/golang/snappy"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/textparse"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// inputFormat identifies how the input stream is encoded.
type inputFormat string

const (
	inAuto        inputFormat = "auto" // sniff the input bytes to decide
	inOpenMetrics inputFormat = "openmetrics"
	inProtobuf    inputFormat = "protobuf"
)

// pbFormat identifies how protobuf MetricFamily messages are framed.
type pbFormat string

const (
	pbDelimited pbFormat = "delimited" // length-prefixed binary protobuf (application/vnd.google.protobuf)
	pbBinary    pbFormat = "binary"    // concatenated/undelimited binary protobuf messages
	pbText      pbFormat = "text"      // protobuf text format, families separated by a blank line
	pbJSON      pbFormat = "json"      // one protobuf JSON object per line
)

// normalizeFormatAlias expands short-hand format aliases ("om" and "pb") to
// their full names ("openmetrics" and "protobuf"). Any other value is
// returned unchanged.
func normalizeFormatAlias(s string) string {
	switch s {
	case "om":
		return string(inOpenMetrics)
	case "pb":
		return string(inProtobuf)
	default:
		return s
	}
}

// detectFormat sniffs raw input bytes to decide whether they are
// OpenMetrics text or one of the binary/text protobuf encodings.
// OpenMetrics (and the protobuf "text"/"json" framings, which are also
// plain text) contain only valid UTF-8 with no control bytes other than
// tab/newline/carriage-return; the "delimited"/"binary" protobuf framings
// are raw binary and almost always contain other control bytes or invalid
// UTF-8 within the first few hundred bytes.
func detectFormat(data []byte) inputFormat {
	sample := data
	if len(sample) > 4096 {
		sample = sample[:4096]
	}
	if !utf8.Valid(sample) {
		return inProtobuf
	}
	for _, b := range sample {
		if b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		if b < 0x20 || b == 0x7f {
			return inProtobuf
		}
	}
	return inOpenMetrics
}

// ---- OpenMetrics text -> []*dto.MetricFamily ----

// metricGroup tracks the per-family state needed to reassemble multi-line
// OpenMetrics series (histogram buckets, summary quantiles, ...) into a
// single dto.Metric keyed by its non-suffix, non-le/quantile label set.
type metricGroup struct {
	family      *dto.MetricFamily
	metricsByID map[string]*dto.Metric
	order       []string
}

// parseOpenMetrics parses OpenMetrics exposition text and groups the
// resulting samples into Prometheus protobuf MetricFamily messages,
// preserving the order in which metric families first appear in the input.
func parseOpenMetrics(data []byte) ([]*dto.MetricFamily, error) {
	st := labels.NewSymbolTable()
	p, err := textparse.New(data, "application/openmetrics-text", st, textparse.ParserOptions{})
	if err != nil {
		return nil, err
	}

	groups := map[string]*metricGroup{}
	var familyOrder []string

	getGroup := func(name string) *metricGroup {
		g, ok := groups[name]
		if !ok {
			g = &metricGroup{
				family: &dto.MetricFamily{
					Name: proto.String(name),
					Type: dto.MetricType_UNTYPED.Enum(),
				},
				metricsByID: map[string]*dto.Metric{},
			}
			groups[name] = g
			familyOrder = append(familyOrder, name)
		}
		return g
	}

	var lbls labels.Labels

	for {
		entry, err := p.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		switch entry {
		case textparse.EntryHelp:
			name, help := p.Help()
			g := getGroup(string(name))
			g.family.Help = proto.String(string(help))

		case textparse.EntryType:
			name, mtype := p.Type()
			g := getGroup(string(name))
			g.family.Type = toProtoType(mtype).Enum()

		case textparse.EntryUnit, textparse.EntryComment:
			// Units and free-form comments have no representation in the
			// classic client_model protobuf; they are intentionally dropped.

		case textparse.EntryHistogram:
			// Native histograms have no representation in client_model
			// (which only supports classic histograms); skip them.

		case textparse.EntrySeries:
			p.Labels(&lbls)
			_, ts, val := p.Series()
			if err := addSeries(groups, getGroup, lbls, val, ts); err != nil {
				return nil, err
			}
		}
	}

	result := make([]*dto.MetricFamily, 0, len(familyOrder))
	for _, name := range familyOrder {
		result = append(result, groups[name].family)
	}
	return result, nil
}

func toProtoType(t model.MetricType) dto.MetricType {
	switch t {
	case model.MetricTypeCounter:
		return dto.MetricType_COUNTER
	case model.MetricTypeGauge:
		return dto.MetricType_GAUGE
	case model.MetricTypeSummary:
		return dto.MetricType_SUMMARY
	case model.MetricTypeHistogram:
		return dto.MetricType_HISTOGRAM
	case model.MetricTypeGaugeHistogram:
		return dto.MetricType_GAUGE_HISTOGRAM
	default:
		return dto.MetricType_UNTYPED
	}
}

// seriesKind classifies an OpenMetrics series name relative to the metric
// family it belongs to (which must already be known, since OpenMetrics
// requires TYPE/HELP lines to precede a metric's samples).
type seriesKind int

const (
	kindPlain seriesKind = iota
	kindBucket
	kindSum
	kindCount
	kindCreated
	kindQuantile
)

// resolveSeries determines which metric family a raw series name belongs to
// (stripping any OpenMetrics-mandated _total/_bucket/_sum/_count/_created
// suffix) and what role the sample plays within that family.
func resolveSeries(rawName string, lbls labels.Labels, groups map[string]*metricGroup) (base string, kind seriesKind) {
	strip := func(suffix string) (string, bool) {
		if strings.HasSuffix(rawName, suffix) {
			cand := strings.TrimSuffix(rawName, suffix)
			if _, ok := groups[cand]; ok {
				return cand, true
			}
		}
		return "", false
	}

	if cand, ok := strip("_bucket"); ok {
		if t := groups[cand].family.GetType(); t == dto.MetricType_HISTOGRAM || t == dto.MetricType_GAUGE_HISTOGRAM {
			return cand, kindBucket
		}
	}
	if cand, ok := strip("_created"); ok {
		return cand, kindCreated
	}
	if cand, ok := strip("_sum"); ok {
		switch groups[cand].family.GetType() {
		case dto.MetricType_HISTOGRAM, dto.MetricType_GAUGE_HISTOGRAM, dto.MetricType_SUMMARY:
			return cand, kindSum
		}
	}
	if cand, ok := strip("_count"); ok {
		switch groups[cand].family.GetType() {
		case dto.MetricType_HISTOGRAM, dto.MetricType_GAUGE_HISTOGRAM, dto.MetricType_SUMMARY:
			return cand, kindCount
		}
	}
	if cand, ok := strip("_total"); ok {
		if groups[cand].family.GetType() == dto.MetricType_COUNTER {
			return cand, kindPlain
		}
	}
	if g, ok := groups[rawName]; ok && g.family.GetType() == dto.MetricType_SUMMARY && lbls.Get("quantile") != "" {
		return rawName, kindQuantile
	}
	return rawName, kindPlain
}

// groupingKey builds a stable identity for a dto.Metric within its family
// from all labels except __name__, le and quantile (which distinguish
// samples belonging to the same histogram/summary observation instead of
// distinct series).
func groupingKey(lbls labels.Labels) string {
	var kept []labels.Label
	lbls.Range(func(l labels.Label) {
		switch l.Name {
		case model.MetricNameLabel, "le", "quantile":
			return
		}
		kept = append(kept, l)
	})
	sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })

	var b strings.Builder
	for _, l := range kept {
		b.WriteString(l.Name)
		b.WriteByte('=')
		b.WriteString(l.Value)
		b.WriteByte('\xff')
	}
	return b.String()
}

func labelPairs(lbls labels.Labels) []*dto.LabelPair {
	var pairs []*dto.LabelPair
	lbls.Range(func(l labels.Label) {
		switch l.Name {
		case model.MetricNameLabel, "le", "quantile":
			return
		}
		pairs = append(pairs, &dto.LabelPair{
			Name:  proto.String(l.Name),
			Value: proto.String(l.Value),
		})
	})
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].GetName() < pairs[j].GetName() })
	return pairs
}

// addSeries records a single parsed OpenMetrics sample into the correct
// dto.Metric, creating the family/metric as needed and merging
// bucket/quantile/sum/count components that arrive as separate series.
func addSeries(groups map[string]*metricGroup, getGroup func(name string) *metricGroup, lbls labels.Labels, val float64, tsMs *int64) error {
	rawName := lbls.Get(model.MetricNameLabel)
	if rawName == "" {
		return fmt.Errorf("series without a metric name: %s", lbls.String())
	}

	base, kind := resolveSeries(rawName, lbls, groups)

	g := getGroup(base)
	key := groupingKey(lbls)
	m, ok := g.metricsByID[key]
	if !ok {
		m = &dto.Metric{Label: labelPairs(lbls)}
		if tsMs != nil {
			m.TimestampMs = tsMs
		}
		g.metricsByID[key] = m
		g.order = append(g.order, key)
		g.family.Metric = append(g.family.Metric, m)
	} else if tsMs != nil {
		m.TimestampMs = tsMs
	}

	switch g.family.GetType() {
	case dto.MetricType_COUNTER:
		ensureCounter(m)
		switch kind {
		case kindCreated:
			m.Counter.CreatedTimestamp = secondsToTimestamp(val)
		default:
			m.Counter.Value = proto.Float64(val)
		}

	case dto.MetricType_GAUGE:
		ensureGauge(m)
		m.Gauge.Value = proto.Float64(val)

	case dto.MetricType_SUMMARY:
		ensureSummary(m)
		switch kind {
		case kindSum:
			m.Summary.SampleSum = proto.Float64(val)
		case kindCount:
			m.Summary.SampleCount = proto.Uint64(uint64(val))
		case kindCreated:
			m.Summary.CreatedTimestamp = secondsToTimestamp(val)
		case kindQuantile:
			q := lbls.Get("quantile")
			qv, err := parseFloat(q)
			if err != nil {
				return fmt.Errorf("invalid quantile label %q: %w", q, err)
			}
			m.Summary.Quantile = append(m.Summary.Quantile, &dto.Quantile{
				Quantile: proto.Float64(qv),
				Value:    proto.Float64(val),
			})
		}

	case dto.MetricType_HISTOGRAM, dto.MetricType_GAUGE_HISTOGRAM:
		ensureHistogram(m)
		switch kind {
		case kindSum:
			m.Histogram.SampleSum = proto.Float64(val)
		case kindCount:
			m.Histogram.SampleCount = proto.Uint64(uint64(val))
		case kindCreated:
			m.Histogram.CreatedTimestamp = secondsToTimestamp(val)
		case kindBucket:
			le := lbls.Get("le")
			upper, err := parseFloat(le)
			if err != nil {
				return fmt.Errorf("invalid le label %q: %w", le, err)
			}
			m.Histogram.Bucket = append(m.Histogram.Bucket, &dto.Bucket{
				UpperBound:      proto.Float64(upper),
				CumulativeCount: proto.Uint64(uint64(val)),
			})
		}

	default: // UNTYPED and anything else (OpenMetrics info/stateset/unknown)
		ensureUntyped(m)
		m.Untyped.Value = proto.Float64(val)
	}

	return nil
}

func ensureCounter(m *dto.Metric) {
	if m.Counter == nil {
		m.Counter = &dto.Counter{}
	}
}

func ensureGauge(m *dto.Metric) {
	if m.Gauge == nil {
		m.Gauge = &dto.Gauge{}
	}
}

func ensureSummary(m *dto.Metric) {
	if m.Summary == nil {
		m.Summary = &dto.Summary{}
	}
}

func ensureHistogram(m *dto.Metric) {
	if m.Histogram == nil {
		m.Histogram = &dto.Histogram{}
	}
}

func ensureUntyped(m *dto.Metric) {
	if m.Untyped == nil {
		m.Untyped = &dto.Untyped{}
	}
}

func parseFloat(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

func secondsToTimestamp(seconds float64) *timestamppb.Timestamp {
	sec := int64(seconds)
	nsec := int64((seconds - float64(sec)) * float64(time.Second))
	return &timestamppb.Timestamp{Seconds: sec, Nanos: int32(nsec)}
}

// ---- protobuf decode ----

// snappyStreamMagic is the fixed 10-byte stream identifier chunk that
// begins every stream produced by the Snappy framing format (see
// https://github.com/google/snappy/blob/main/framing_format.txt): a
// stream identifier chunk (type 0xff, length 6) containing "sNaPpY".
var snappyStreamMagic = []byte{0xff, 0x06, 0x00, 0x00, 's', 'N', 'a', 'P', 'p', 'Y'}

// maybeDecompressSnappy transparently decompresses data framed with the
// Snappy stream format (as written by "omtool convert -to protobuf
// -snappy"), detected by its fixed magic prefix. Input without that prefix
// is returned unchanged, so plain (uncompressed) protobuf input keeps
// working without a flag.
func maybeDecompressSnappy(data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, snappyStreamMagic) {
		return data, nil
	}
	decompressed, err := io.ReadAll(snappy.NewReader(bytes.NewReader(data)))
	if err != nil {
		return nil, fmt.Errorf("decompressing snappy-framed input: %w", err)
	}
	return decompressed, nil
}

// decodeProtobuf parses raw bytes into MetricFamily messages according to
// the given framing. Input is first checked for the Snappy frame format
// magic prefix and transparently decompressed if present, so
// Snappy-compressed protobuf input (delimited or binary) is auto-detected
// without needing a dedicated flag.
func decodeProtobuf(data []byte, format pbFormat) ([]*dto.MetricFamily, error) {
	data, err := maybeDecompressSnappy(data)
	if err != nil {
		return nil, err
	}
	switch format {
	case pbDelimited:
		return decodeDelimited(data)
	case pbBinary:
		return decodeBinary(data)
	case pbText:
		return decodeText(data)
	case pbJSON:
		return decodeJSON(data)
	default:
		return nil, fmt.Errorf("unknown protobuf format %q (want delimited|binary|text|json)", format)
	}
}

func decodeDelimited(data []byte) ([]*dto.MetricFamily, error) {
	dec := expfmt.NewDecoder(bytes.NewReader(data), expfmt.NewFormat(expfmt.TypeProtoDelim))
	var families []*dto.MetricFamily
	for {
		mf := &dto.MetricFamily{}
		if err := dec.Decode(mf); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		families = append(families, mf)
	}
	return families, nil
}

// decodeBinary parses a single, undelimited binary protobuf MetricFamily
// message. Concatenated undelimited messages carry no boundary marker, so
// unmarshaling them as one message silently merges the families per normal
// protobuf semantics (repeated fields like "metric" are appended together,
// singular fields like "name"/"help"/"type" end up holding the last
// family's value) rather than failing outright. This format is therefore
// only reliable for a single metric family per file; use "delimited" for
// files with more than one family.
func decodeBinary(data []byte) ([]*dto.MetricFamily, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	mf := &dto.MetricFamily{}
	if err := proto.Unmarshal(data, mf); err != nil {
		return nil, fmt.Errorf("parsing undelimited binary protobuf (only a single metric family is supported; use -pb-format delimited for multiple): %w", err)
	}
	return []*dto.MetricFamily{mf}, nil
}

// decodeText parses protobuf text format records as written by
// "omtool convert -to protobuf -pb-format text", where each MetricFamily
// is separated from the next by a blank line.
func decodeText(data []byte) ([]*dto.MetricFamily, error) {
	blocks := strings.Split(string(data), "\n\n")
	var families []*dto.MetricFamily
	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		mf := &dto.MetricFamily{}
		if err := prototext.Unmarshal([]byte(block), mf); err != nil {
			return nil, err
		}
		families = append(families, mf)
	}
	return families, nil
}

// decodeJSON parses one protobuf JSON object per non-empty line, as written
// by "omtool convert -to protobuf -pb-format json".
func decodeJSON(data []byte) ([]*dto.MetricFamily, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	var families []*dto.MetricFamily
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		mf := &dto.MetricFamily{}
		if err := protojson.Unmarshal([]byte(line), mf); err != nil {
			return nil, err
		}
		families = append(families, mf)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return families, nil
}

// readFamilies reads data and decodes it into MetricFamily messages using
// the given input format (resolving inAuto via detectFormat first) and
// protobuf framing (used only when the resolved format is inProtobuf).
func readFamilies(data []byte, from inputFormat, pbFmt pbFormat) ([]*dto.MetricFamily, error) {
	if from == inAuto {
		from = detectFormat(data)
	}
	switch from {
	case inOpenMetrics:
		families, err := parseOpenMetrics(data)
		if err != nil {
			return nil, fmt.Errorf("parsing OpenMetrics input: %w", err)
		}
		return families, nil
	case inProtobuf:
		families, err := decodeProtobuf(data, pbFmt)
		if err != nil {
			return nil, fmt.Errorf("decoding protobuf input: %w", err)
		}
		return families, nil
	default:
		return nil, fmt.Errorf("unknown input format %q (want auto|openmetrics(om)|protobuf(pb))", from)
	}
}
