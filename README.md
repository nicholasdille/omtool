# omtool

A single Go CLI, built around one shared OpenMetrics/protobuf parser
internal to `cmd/omtool`, with two subcommands:

- **omtool convert** converts between [OpenMetrics](https://openmetrics.io/)
  text exposition format and Prometheus protobuf `MetricFamily` messages
  (`io.prometheus.client`, aka `client_model`), in either direction, plus a
  human readable output mode for inspection/debugging.
- **omtool send** sends metrics from a file to a Prometheus- or
  Mimir-compatible remote-write endpoint, as a Snappy-compressed protobuf
  `WriteRequest`.

## Build

```sh
go build -o omtool ./cmd/omtool
```

## Usage

```sh
./omtool convert -in metrics.om -to protobuf -pb-format delimited -out metrics.pb
./omtool convert -in metrics.pb -from protobuf -to human
```

All functionality lives under the `convert` subcommand. Flags:

- `-in` — input file (`-` for stdin, default).
- `-out` — output file (`-` for stdout, default).
- `-from` — input format:
  - `auto` (default) — sniff the input: valid, control-byte-free UTF-8 is
    treated as OpenMetrics text; anything else as protobuf.
  - `openmetrics` — OpenMetrics text exposition format.
  - `protobuf` — protobuf `MetricFamily` messages, framed per `-pb-format`.
    Snappy-framed (stream format) protobuf input is auto-detected by its
    magic prefix and transparently decompressed before decoding, for both
    `auto` and explicit `protobuf` input formats — no extra flag needed.
- `-to` — output format:
  - `openmetrics` — OpenMetrics text exposition format.
  - `protobuf` — protobuf `MetricFamily` messages, framed per `-pb-format`.
  - `human` (default) — indented plain text, one block per metric family,
    with labels and values (including histogram buckets and summary
    quantiles) spelled out.
- `-pb-format` — protobuf framing, used whenever `-from=protobuf` and/or
  `-to=protobuf`:
  - `delimited` (default) — length-prefixed binary protobuf, the standard
    Prometheus `application/vnd.google.protobuf` scrape format.
  - `binary` — a single, undelimited binary protobuf message. Because
    concatenated undelimited messages have no boundary marker, this only
    reliably supports **one** metric family; use `delimited` for several.
  - `text` — protobuf text format, families separated by a blank line.
  - `json` — one protobuf JSON object per line.

Piping through stdin/stdout also works:

```sh
curl -s -H 'Accept: application/openmetrics-text' http://localhost:9100/metrics \
  | ./omtool convert -to protobuf -pb-format delimited > metrics.pb
```

Round-tripping OpenMetrics through protobuf back to human readable text:

```sh
./omtool convert -in metrics.om -to protobuf | ./omtool convert -from protobuf -to human
```

## Conversion notes

- Counters, gauges, histograms, summaries and untyped samples are supported.
  Samples for the same histogram/summary (buckets, quantiles, `_sum`,
  `_count`) are merged back into a single protobuf `Metric` based on their
  shared labels, per the OpenMetrics suffix conventions (`_total`,
  `_bucket`, `_sum`, `_count`, `_created`).
- `_created` timestamps are mapped to the protobuf `created_timestamp`
  field where supported (counter, summary, histogram).
- OpenMetrics native histograms, `UNIT` lines, and comments have no
  equivalent in the classic `client_model` protobuf schema and are dropped
  when converting from OpenMetrics.

## omtool send

Sends a file (OpenMetrics text, or protobuf `MetricFamily` in any of
`omtool convert`'s framings, Snappy-stream-compressed or not — same
auto-detection as `omtool convert`'s `-from`/`-pb-format`) to a Prometheus
or Mimir remote-write endpoint. It converts every metric family into
`prometheus.WriteRequest` `TimeSeries` (expanding histogram buckets and
summary quantiles into their own `_bucket`/`le` and `quantile`-labelled
series, the same way Prometheus itself does), marshals it to protobuf,
compresses it with block-format Snappy (as the remote-write protocol
requires — distinct from the Snappy *stream* format used elsewhere for
at-rest files), and `POST`s it with the standard remote-write headers.

```sh
# Prometheus
./omtool send -in metrics.om -url http://localhost:9090/api/v1/write

# Mimir (multi-tenant; X-Scope-OrgID is required unless auth is disabled)
./omtool send -in metrics.pb -from protobuf -pb-format delimited \
  -url http://localhost:8080/api/v1/push -target mimir -tenant-id my-tenant
```

Flags:

- `-in` — input file (`-` for stdin, default).
- `-from` / `-pb-format` — same meaning as in `omtool convert`.
- `-url` — remote-write endpoint URL (required unless `-dry-run`).
- `-target` — `prometheus` (default) or `mimir`; only used to warn if
  `-tenant-id` is missing for Mimir.
- `-tenant-id` — sets `X-Scope-OrgID` (Mimir tenant/org ID).
- `-username` / `-password` — HTTP basic auth.
- `-bearer-token` — `Authorization: Bearer` token (overrides basic auth).
- `-header` — extra `"Key: Value"` header, repeatable.
- `-timeout` — HTTP request timeout (default `30s`).
- `-insecure-skip-verify` — skip TLS certificate verification.
- `-dry-run` — build and report the payload without sending it.

Samples without an explicit timestamp (e.g. plain OpenMetrics samples) are
stamped with the current time at send time.
