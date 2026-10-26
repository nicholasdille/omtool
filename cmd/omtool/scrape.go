package main

// This file implements the "scrape" subcommand: scraping an HTTP or HTTPS
// endpoint that exposes its metrics as text exposition (OpenMetrics, or the
// classic "text/plain; version=0.0.4" Prometheus format), parsing the
// response body and writing the decoded metric families out in any of
// "omtool convert"'s output formats.

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/spf13/cobra"
)

// scrapeAcceptHeader is the Accept header every scrape request advertises,
// in the standard Prometheus scrape negotiation order: OpenMetrics text
// first, with classic text exposition as a weaker fallback for endpoints
// that do not implement OpenMetrics negotiation. Extra -header values can
// override it (they are applied after this default).
const scrapeAcceptHeader = "application/openmetrics-text; version=1.0.0, text/plain; version=0.0.4; q=0.5"

// newScrapeCmd builds the "scrape" subcommand: it fetches metric data from an
// HTTP(S) endpoint and parses it as OpenMetrics/classic text exposition.
func newScrapeCmd() *cobra.Command {
	var (
		url, outPath, to, pbFmtFl string
		username, password        string
		bearerToken, userAgent    string
		timeout                   time.Duration
		insecureSkipVerify        bool
		extraHeaders              headerList
	)

	cmd := &cobra.Command{
		Use:   "scrape",
		Short: "Scrape an HTTP(S) endpoint exposing OpenMetrics text and convert its output",
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := runScrape(url, outPath, outputFormat(normalizeFormatAlias(to)), pbFormat(pbFmtFl), username, password, bearerToken, userAgent, timeout, insecureSkipVerify, extraHeaders); err != nil {
				return fmt.Errorf("omtool scrape: %w", err)
			}
			return nil
		},
	}

	fs := cmd.Flags()
	fs.StringVar(&url, "url", "", "HTTP(S) endpoint to scrape, required; typically a /metrics path")
	fs.StringVar(&outPath, "out", "-", `output file ("-" for stdout)`)
	fs.StringVar(&to, "to", string(outOpenMetrics), "output format: openmetrics(om)|protobuf(pb)|human")
	fs.StringVar(&pbFmtFl, "pb-format", string(pbBinary), "protobuf framing, for --to=protobuf: delimited|binary|text|json")
	fs.StringVar(&username, "username", "", "username for HTTP basic auth; omitted if empty")
	fs.StringVar(&password, "password", "", "password for HTTP basic auth")
	fs.StringVar(&bearerToken, "bearer-token", "", "bearer token for Authorization header; omitted if empty (overrides --username/--password)")
	fs.StringVar(&userAgent, "user-agent", "omtool-scrape/1.0", "value for the User-Agent header")
	fs.DurationVar(&timeout, "timeout", 30*time.Second, "HTTP request timeout")
	fs.BoolVar(&insecureSkipVerify, "insecure-skip-verify", false, "skip TLS certificate verification")
	fs.Var(&extraHeaders, "header", `extra HTTP header "Key: Value" (repeatable; may override the default Accept header)`)
	_ = cmd.MarkFlagRequired("url")

	return cmd
}

func runScrape(url, outPath string, to outputFormat, pbFmt pbFormat, username, password, bearerToken, userAgent string, timeout time.Duration, insecureSkipVerify bool, extraHeaders headerList) error {
	data, contentType, err := scrapeRequest(url, scrapeAcceptHeader, username, password, bearerToken, userAgent, timeout, insecureSkipVerify, extraHeaders)
	if err != nil {
		return err
	}

	families, err := parseScrapeResponse(data, contentType)
	if err != nil {
		return err
	}

	nSeries := 0
	for _, mf := range families {
		nSeries += len(mf.GetMetric())
	}
	log.Printf("omtool scrape: %s: %d metric families, %d series", url, len(families), nSeries)

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
		err = writeProtobuf(w, families, pbFmt)
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

// scrapeRequest GETs url, returning the response body and its Content-Type.
// Only the status is negotiated up front (non-2xx statuses fail with the
// first 8KiB of the body as context); the body itself is returned whole
// because it is the payload being parsed.
func scrapeRequest(url, accept, username, password, bearerToken, userAgent string, timeout time.Duration, insecureSkipVerify bool, extraHeaders headerList) ([]byte, string, error) {
	req, err := http.NewRequest(http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", userAgent)
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	} else if username != "" {
		req.SetBasicAuth(username, password)
	}
	for _, h := range extraHeaders {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return nil, "", fmt.Errorf("invalid -header %q (want \"Key: Value\")", h)
		}
		req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}

	client := &http.Client{Timeout: timeout}
	if insecureSkipVerify {
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // opt-in via -insecure-skip-verify
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("scraping %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.Header.Get("Content-Type"), fmt.Errorf("reading scrape response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		excerpt := data
		if len(excerpt) > 8192 {
			excerpt = excerpt[:8192]
		}
		return nil, resp.Header.Get("Content-Type"), fmt.Errorf("scrape endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(excerpt)))
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// parseScrapeResponse parses a scrape response body into MetricFamily
// messages, choosing the text exposition parser from the response's
// Content-Type per scrapeMediaType, and requires at least one metric family
// (an otherwise empty response is treated as an error, matching "send"'s
// behavior for empty input).
func parseScrapeResponse(data []byte, contentType string) ([]*dto.MetricFamily, error) {
	families, err := parseTextFamilies(data, scrapeMediaType(contentType, data))
	if err != nil {
		return nil, fmt.Errorf("parsing scrape response: %w", err)
	}
	if len(families) == 0 {
		return nil, fmt.Errorf("no metric families found in scrape response (Content-Type %q)", contentType)
	}
	return families, nil
}

// scrapeMediaType picks the media type to parse the scrape response as.
// An OpenMetrics content type parses as strict OpenMetrics (including the
// mandatory trailing "# EOF" marker, as "omtool validate" enforces), classic
// "text/plain" content types parse as classic Prometheus text, and anything
// else (including a missing Content-Type) is sniffed by whether the body
// ends in the "# EOF" marker only an OpenMetrics response carries.
func scrapeMediaType(contentType string, data []byte) string {
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "openmetrics"):
		return openmetricsContentType
	case strings.HasPrefix(ct, "text/plain"):
		return "text/plain"
	}
	if strings.HasSuffix(strings.TrimRight(string(data), " \t\r\n"), "# EOF") {
		return openmetricsContentType
	}
	return "text/plain"
}
