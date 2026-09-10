# omtool

A single Go CLI, built around one shared OpenMetrics/protobuf parser
internal to `cmd/omtool`, with three subcommands:

- **omtool convert** converts between [OpenMetrics](https://openmetrics.io/)
  text exposition format and Prometheus protobuf `MetricFamily` messages
  (`io.prometheus.client`, aka `client_model`), in either direction, plus a
  human readable output mode for inspection/debugging.
- **omtool send** sends metrics from a file to a Prometheus- or
  Mimir-compatible remote-write endpoint, as a Snappy-compressed protobuf
  `WriteRequest`.
- **omtool validate** checks that an input file is well-formed
  [OpenMetrics](https://openmetrics.io/) text exposition format.

## Build

```sh
go build -o omtool ./cmd/omtool
```

## Testing

```sh
go test ./...
```

To see test coverage:

```sh
go test -cover ./...
```

## Usage

```sh
./omtool convert --in metrics.om --to protobuf --pb-format delimited --out metrics.pb
./omtool convert --in metrics.pb --from protobuf --to human
```

Flags for `omtool convert`:

- `--in` — input file, required (use `-` for stdin).
- `--out` — output file (use `-` for stdout; default `-`).
- `--from` — input format:
  - `auto` (default) — sniff the input: valid, control-byte-free UTF-8 is
    treated as OpenMetrics text; anything else as protobuf.
  - `openmetrics` (alias `om`) — OpenMetrics text exposition format.
  - `protobuf` (alias `pb`) — protobuf `MetricFamily` messages, framed per
    `--pb-format`. Snappy-framed (stream format) protobuf input is
    auto-detected by its magic prefix and transparently decompressed
    before decoding, for both `auto` and explicit `protobuf` input
    formats — no extra flag needed.
- `--to` — output format:
  - `openmetrics` (alias `om`) — OpenMetrics text exposition format.
  - `protobuf` (alias `pb`) — protobuf `MetricFamily` messages, framed per
    `--pb-format`.
  - `human` (default) — indented plain text, one block per metric family,
    with labels and values (including histogram buckets and summary
    quantiles) spelled out.
- `--pb-format` — protobuf framing, used whenever `--from=protobuf` and/or
  `--to=protobuf`:
  - `binary` (default) — a single, undelimited binary protobuf message.
    Because concatenated undelimited messages have no boundary marker,
    this only reliably supports **one** metric family; use `delimited`
    for several.
  - `delimited` — length-prefixed binary protobuf, the standard
    Prometheus `application/vnd.google.protobuf` scrape format.
  - `text` — protobuf text format, families separated by a blank line.
  - `json` — one protobuf JSON object per line.
- `--snappy` — compress protobuf output with Snappy (only applies to
  `--to=protobuf`).

Piping through stdin/stdout also works (pass `-` explicitly, since `--in` is required):

```sh
curl -s -H 'Accept: application/openmetrics-text' http://localhost:9100/metrics \
  | ./omtool convert --in - --to protobuf --pb-format delimited --out metrics.pb
```

Round-tripping OpenMetrics through protobuf back to human readable text:

```sh
./omtool convert --in metrics.om --to protobuf --out - | ./omtool convert --in - --from protobuf --to human --out -
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
auto-detection as `omtool convert`'s `--from`/`--pb-format`) to a Prometheus
or Mimir remote-write endpoint. It converts every metric family into
`prometheus.WriteRequest` `TimeSeries` (expanding histogram buckets and
summary quantiles into their own `_bucket`/`le` and `quantile`-labelled
series, the same way Prometheus itself does), marshals it to protobuf,
compresses it with block-format Snappy (as the remote-write protocol
requires — distinct from the Snappy *stream* format used elsewhere for
at-rest files), and `POST`s it with the standard remote-write headers.

```sh
# Prometheus
./omtool send --in metrics.om --url http://localhost:9090/api/v1/write

# Mimir (multi-tenant; X-Scope-OrgID is required unless auth is disabled)
./omtool send --in metrics.pb --from protobuf --pb-format delimited \
  --url http://localhost:8080/api/v1/push --target mimir --tenant-id my-tenant
```

Flags for `omtool send`:

- `--in` — input file, required (use `-` for stdin).
- `--from` / `--pb-format` — same meaning as in `omtool convert`.
- `--url` — remote-write endpoint URL; defaults to
  `http://localhost:9090/api/v1/write` for `--target=prometheus` or
  `http://localhost:8080/api/v1/push` for `--target=mimir`.
- `--target` — `prometheus` (default) or `mimir`; only used to warn if
  `--tenant-id` is missing for Mimir.
- `--tenant-id` — sets `X-Scope-OrgID` (Mimir tenant/org ID); defaults to
  `anonymous`, omitted from the request entirely if set to `""`.
- `--username` / `--password` — HTTP basic auth.
- `--bearer-token` — `Authorization: Bearer` token (overrides basic auth).
- `--user-agent` — value for the `User-Agent` header (default
  `omtool-send/1.0`).
- `--header` — extra `"Key: Value"` header, repeatable.
- `--timeout` — HTTP request timeout (default `30s`).
- `--insecure-skip-verify` — skip TLS certificate verification.
- `--dry-run` — build and report the payload without sending it.

Samples without an explicit timestamp (e.g. plain OpenMetrics samples) are
stamped with the current time at send time.

## omtool validate

Checks that an input file is well-formed
[OpenMetrics](https://openmetrics.io/) text exposition format, using the
same strict parser as `omtool convert`/`omtool send` (which enforces the
format's rules, including the mandatory trailing `# EOF` marker). On
success it prints a one-line summary of how many metric families and
series were found and exits `0`; on failure it prints the parse error to
stderr and exits non-zero.

```sh
./omtool validate --in metrics.om
```

Flags for `omtool validate`:

- `--in` — input file, required (use `-` for stdin).
- `--quiet` — suppress the success summary; print nothing on success
  (useful for validation in scripts, where only the exit code matters).

## Example

Here is an example of a simple OpenMetrics file:

```plaintext
$ cat up.om
# HELP up Whether a service is running
# TYPE up gauge
up{service="bar"} 1
# EOF
```

This will validate the `up.om` file and print a summary of the metric families and series found. If the file is well-formed, the command will exit with a status code of `0`; otherwise, it will print the parse error and exit with a non-zero status code.

```sh
$ omtool validate --in up.om
```

To simulate errors in the validation, you can introduce small errors like removing the mandatory `# EOF` marker or removing the metrics name `up` from the `TYPE` line.

Convert `up.om` to protobuf:

```sh
$ omtool convert --from om --in up.om --to protobuf --out up.pb
```

Use snappy compression on the protobuf output:

```sh
$ omtool convert --from om --in up.om --to protobuf --out up.pb --snappy
```

Send the protobuf file to a remote endpoint:

```sh
$ omtool send --in up.pb --target mimir
```
