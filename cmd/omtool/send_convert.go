package main

import (
	"sort"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/prometheus/prompb"
)

// buildWriteRequest converts decoded MetricFamily messages into a
// Prometheus remote-write WriteRequest: one TimeSeries per sample (with
// histogram buckets and summary quantiles expanded into their own
// "_bucket"/"le" and "quantile"-labelled series, following the same naming
// convention Prometheus itself uses when scraping and remote-writing
// classic histograms/summaries) plus one MetricMetadata entry per family.
//
// Samples without an explicit protobuf timestamp are stamped with
// defaultTS (milliseconds since the Unix epoch), typically time.Now().
func buildWriteRequest(families []*dto.MetricFamily, defaultTS int64) *prompb.WriteRequest {
	wr := &prompb.WriteRequest{}

	for _, fam := range families {
		name := fam.GetName()
		if name == "" {
			continue
		}

		wr.Metadata = append(wr.Metadata, prompb.MetricMetadata{
			Type:             toMetadataType(fam.GetType()),
			MetricFamilyName: name,
			Help:             fam.GetHelp(),
			Unit:             fam.GetUnit(),
		})

		for _, m := range fam.GetMetric() {
			ts := m.GetTimestampMs()
			if ts == 0 {
				ts = defaultTS
			}
			base := labelPairsToPrompb(m.GetLabel())

			switch fam.GetType() {
			case dto.MetricType_COUNTER:
				appendSeries(wr, name, base, m.GetCounter().GetValue(), ts)

			case dto.MetricType_GAUGE:
				appendSeries(wr, name, base, m.GetGauge().GetValue(), ts)

			case dto.MetricType_UNTYPED:
				appendSeries(wr, name, base, m.GetUntyped().GetValue(), ts)

			case dto.MetricType_SUMMARY:
				s := m.GetSummary()
				for _, q := range s.GetQuantile() {
					lbls := withExtraLabel(base, "quantile", formatFloat(q.GetQuantile()))
					appendSeries(wr, name, lbls, q.GetValue(), ts)
				}
				appendSeries(wr, name+"_sum", base, s.GetSampleSum(), ts)
				appendSeries(wr, name+"_count", base, float64(s.GetSampleCount()), ts)

			case dto.MetricType_HISTOGRAM, dto.MetricType_GAUGE_HISTOGRAM:
				h := m.GetHistogram()
				for _, b := range h.GetBucket() {
					lbls := withExtraLabel(base, "le", formatFloat(b.GetUpperBound()))
					count := b.GetCumulativeCountFloat()
					if count == 0 {
						count = float64(b.GetCumulativeCount())
					}
					appendSeries(wr, name+"_bucket", lbls, count, ts)
				}
				appendSeries(wr, name+"_sum", base, h.GetSampleSum(), ts)
				count := h.GetSampleCountFloat()
				if count == 0 {
					count = float64(h.GetSampleCount())
				}
				appendSeries(wr, name+"_count", base, count, ts)
			}
		}
	}

	return wr
}

// appendSeries appends a single-sample TimeSeries named seriesName, with
// labels sorted by name (required by the remote-write wire format and
// enforced by both Prometheus and Mimir) and a "__name__" label added.
func appendSeries(wr *prompb.WriteRequest, seriesName string, extra []prompb.Label, value float64, ts int64) {
	lbls := make([]prompb.Label, 0, len(extra)+1)
	lbls = append(lbls, prompb.Label{Name: "__name__", Value: seriesName})
	lbls = append(lbls, extra...)
	sort.Slice(lbls, func(i, j int) bool { return lbls[i].Name < lbls[j].Name })

	wr.Timeseries = append(wr.Timeseries, prompb.TimeSeries{
		Labels:  lbls,
		Samples: []prompb.Sample{{Value: value, Timestamp: ts}},
	})
}

// labelPairsToPrompb converts client_model label pairs to prompb labels,
// excluding "__name__" (the series name is added separately by
// appendSeries) since a metric's own label set never legitimately carries
// it.
func labelPairsToPrompb(pairs []*dto.LabelPair) []prompb.Label {
	lbls := make([]prompb.Label, 0, len(pairs))
	for _, p := range pairs {
		if p.GetName() == "__name__" {
			continue
		}
		lbls = append(lbls, prompb.Label{Name: p.GetName(), Value: p.GetValue()})
	}
	return lbls
}

// withExtraLabel returns a copy of base with an additional label appended
// (used for the synthetic "le"/"quantile" labels), leaving base untouched
// so it can be reused across a histogram/summary's several derived series.
func withExtraLabel(base []prompb.Label, name, value string) []prompb.Label {
	lbls := make([]prompb.Label, len(base), len(base)+1)
	copy(lbls, base)
	return append(lbls, prompb.Label{Name: name, Value: value})
}

// toMetadataType maps a client_model MetricType to its prompb
// MetricMetadata_MetricType equivalent.
func toMetadataType(t dto.MetricType) prompb.MetricMetadata_MetricType {
	switch t {
	case dto.MetricType_COUNTER:
		return prompb.MetricMetadata_COUNTER
	case dto.MetricType_GAUGE:
		return prompb.MetricMetadata_GAUGE
	case dto.MetricType_SUMMARY:
		return prompb.MetricMetadata_SUMMARY
	case dto.MetricType_HISTOGRAM:
		return prompb.MetricMetadata_HISTOGRAM
	case dto.MetricType_GAUGE_HISTOGRAM:
		return prompb.MetricMetadata_GAUGEHISTOGRAM
	default:
		return prompb.MetricMetadata_UNKNOWN
	}
}
