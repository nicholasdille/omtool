package main

// The "send" subcommand sends metrics from a file (OpenMetrics text or
// Prometheus protobuf MetricFamily, in any framing "omtool convert" can
// produce, optionally Snappy-compressed) to a Prometheus/Mimir
// remote-write endpoint: it decodes the input, converts it into a
// protobuf prometheus.WriteRequest, Snappy-block-compresses it (the
// compression the remote-write protocol itself requires, distinct from
// the Snappy stream/frame format used elsewhere in this tool for at-rest
// files), and POSTs it with the headers the remote-write protocol
// expects.

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/spf13/cobra"
)

// remoteWriteVersion is the value of the mandatory
// X-Prometheus-Remote-Write-Version header, per the remote-write protocol.
const remoteWriteVersion = "0.1.0"

// defaultURLs maps -target values to their conventional local remote-write
// endpoint, used when -url is left empty.
var defaultURLs = map[string]string{
	"prometheus": "http://localhost:9090/api/v1/write",
	"mimir":      "http://localhost:8080/api/v1/push",
}

// headerList collects repeated -header "Key: Value" flags.
type headerList []string

func (h *headerList) String() string { return strings.Join(*h, ",") }
func (h *headerList) Set(v string) error {
	*h = append(*h, v)
	return nil
}
func (h *headerList) Type() string { return "stringArray" }

// newSendCmd builds the "send" subcommand: it ships converted metric data
// to a remote-write endpoint.
func newSendCmd() *cobra.Command {
	var (
		inPath, from, pbFmtFl string
		url, target           string
		tenantID              string
		username, password    string
		bearerToken           string
		userAgent             string
		timeout               time.Duration
		insecureSkipVerify    bool
		dryRun                bool
		extraHeaders          headerList
	)

	cmd := &cobra.Command{
		Use:   "send",
		Short: "Send metric data to a Prometheus/Mimir remote-write endpoint",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runSend(inPath, inputFormat(normalizeFormatAlias(from)), pbFormat(pbFmtFl), url, target, tenantID, username, password, bearerToken, userAgent, timeout, insecureSkipVerify, dryRun, extraHeaders); err != nil {
				return fmt.Errorf("omtool send: %w", err)
			}
			return nil
		},
	}

	fs := cmd.Flags()
	fs.StringVar(&inPath, "in", "", `input file ("-" for stdin)`)
	fs.StringVar(&from, "from", string(inAuto), "input format: auto|openmetrics(om)|protobuf(pb)")
	fs.StringVar(&pbFmtFl, "pb-format", string(pbBinary), "protobuf framing, for --from=protobuf: delimited|binary|text|json")
	fs.StringVar(&url, "url", "", "remote-write endpoint URL; defaults to http://localhost:9090/api/v1/write for -target=prometheus or http://localhost:8080/api/v1/push for -target=mimir")
	fs.StringVar(&target, "target", "prometheus", "target backend, used only to sanity-check flags: prometheus|mimir (Mimir requires -tenant-id unless multi-tenancy is disabled)")
	fs.StringVar(&tenantID, "tenant-id", "anonymous", "value for the X-Scope-OrgID header (Mimir tenant/org ID); omitted if empty")
	fs.StringVar(&username, "username", "", "username for HTTP basic auth; omitted if empty")
	fs.StringVar(&password, "password", "", "password for HTTP basic auth")
	fs.StringVar(&bearerToken, "bearer-token", "", "bearer token for Authorization header; omitted if empty (overrides --username/--password)")
	fs.StringVar(&userAgent, "user-agent", "omtool-send/1.0", "value for the User-Agent header")
	fs.DurationVar(&timeout, "timeout", 30*time.Second, "HTTP request timeout")
	fs.BoolVar(&insecureSkipVerify, "insecure-skip-verify", false, "skip TLS certificate verification")
	fs.BoolVar(&dryRun, "dry-run", false, "build and report the payload but do not send it")
	fs.Var(&extraHeaders, "header", `extra HTTP header "Key: Value" (repeatable)`)
	_ = cmd.MarkFlagRequired("in")

	return cmd
}

func runSend(inPath string, from inputFormat, pbFmt pbFormat, url, target, tenantID, username, password, bearerToken, userAgent string, timeout time.Duration, insecureSkipVerify, dryRun bool, extraHeaders headerList) error {
	target = strings.ToLower(target)
	switch target {
	case "prometheus", "mimir":
	default:
		return fmt.Errorf("unknown -target %q (want prometheus|mimir)", target)
	}
	if target == "mimir" && tenantID == "" {
		log.Printf("omtool send: warning: -target=mimir without -tenant-id; this only works if Mimir multi-tenancy (auth) is disabled")
	}
	if url == "" {
		url = defaultURLs[target]
	}
	if !dryRun && url == "" {
		return fmt.Errorf("--url is required (unless --dry-run is set)")
	}

	families, err := loadFamilies(inPath, from, pbFmt)
	if err != nil {
		return err
	}
	if len(families) == 0 {
		return fmt.Errorf("no metric families decoded from input")
	}

	wr := buildWriteRequest(families, time.Now().UnixMilli())

	raw, err := proto.Marshal(wr)
	if err != nil {
		return fmt.Errorf("marshaling WriteRequest: %w", err)
	}
	// The remote-write protocol requires raw (block-format) Snappy
	// compression of the marshaled protobuf message, not the Snappy
	// stream/frame format used by "omtool convert -to protobuf -snappy".
	compressed := snappy.Encode(nil, raw)

	nSeries, nSamples := len(wr.Timeseries), 0
	for _, ts := range wr.Timeseries {
		nSamples += len(ts.Samples)
	}
	log.Printf("omtool send: %d metric families -> %d time series, %d samples (%d bytes protobuf, %d bytes snappy-compressed)",
		len(families), nSeries, nSamples, len(raw), len(compressed))

	if dryRun {
		log.Printf("omtool send: -dry-run set, not sending")
		return nil
	}

	return sendRequest(url, compressed, tenantID, username, password, bearerToken, userAgent, timeout, insecureSkipVerify, extraHeaders)
}

func sendRequest(url string, body []byte, tenantID, username, password, bearerToken, userAgent string, timeout time.Duration, insecureSkipVerify bool, extraHeaders headerList) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Prometheus-Remote-Write-Version", remoteWriteVersion)
	req.Header.Set("User-Agent", userAgent)
	if tenantID != "" {
		req.Header.Set("X-Scope-OrgID", tenantID)
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	} else if username != "" {
		req.SetBasicAuth(username, password)
	}
	for _, h := range extraHeaders {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return fmt.Errorf("invalid -header %q (want \"Key: Value\")", h)
		}
		req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}

	client := &http.Client{Timeout: timeout}
	if insecureSkipVerify {
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // opt-in via -insecure-skip-verify
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("remote-write endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	log.Printf("omtool send: %s -> %s", url, resp.Status)
	return nil
}
