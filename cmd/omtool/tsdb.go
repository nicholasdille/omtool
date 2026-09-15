package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/prometheus/common/promslog"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/spf13/cobra"
)

func newTSDBCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "tsdb", Short: "Manage Prometheus TSDB blocks"}
	cmd.AddCommand(newTSDBCreateCmd(), newTSDBBackfillCmd())
	return cmd
}

func newTSDBCreateCmd() *cobra.Command {
	var inPath, outPath, from, pbFmtFl string
	var blockDuration time.Duration
	cmd := &cobra.Command{
		Use: "create", Short: "Convert metric data to Prometheus TSDB blocks",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runTSDB(inPath, outPath, inputFormat(normalizeFormatAlias(from)), pbFormat(pbFmtFl), blockDuration); err != nil {
				return fmt.Errorf("omtool tsdb create: %w", err)
			}
			return nil
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&inPath, "in", "", `input file ("-" for stdin)`)
	fs.StringVar(&outPath, "out", "", "output directory for TSDB blocks")
	fs.StringVar(&from, "from", string(inAuto), "input format: auto|openmetrics(om)|protobuf(pb)")
	fs.StringVar(&pbFmtFl, "pb-format", string(pbBinary), "protobuf framing: delimited|binary|text|json")
	fs.DurationVar(&blockDuration, "block-duration", 2*time.Hour, "TSDB block duration")
	_ = cmd.MarkFlagRequired("in")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

func newTSDBBackfillCmd() *cobra.Command {
	var inPath, endpoint, tenantID, username, password, bearerToken, userAgent string
	var timeout, pollInterval time.Duration
	var insecureSkipVerify, dryRun bool
	var extraHeaders headerList
	cmd := &cobra.Command{
		Use: "backfill", Short: "Backfill a TSDB block to Mimir",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runTSDBBackfill(inPath, endpoint, tenantID, username, password, bearerToken, userAgent, timeout, pollInterval, insecureSkipVerify, dryRun, extraHeaders); err != nil {
				return fmt.Errorf("omtool tsdb backfill: %w", err)
			}
			return nil
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&inPath, "in", "", "TSDB block directory to upload")
	fs.StringVar(&endpoint, "url", "http://localhost:8080", "Mimir base URL")
	fs.StringVar(&tenantID, "tenant-id", "anonymous", "value for the X-Scope-OrgID header; omitted if empty")
	fs.StringVar(&username, "username", "", "username for HTTP basic auth; omitted if empty")
	fs.StringVar(&password, "password", "", "password for HTTP basic auth")
	fs.StringVar(&bearerToken, "bearer-token", "", "bearer token; omitted if empty (overrides basic auth)")
	fs.StringVar(&userAgent, "user-agent", "omtool-tsdb/1.0", "value for the User-Agent header")
	fs.DurationVar(&timeout, "timeout", 30*time.Second, "HTTP request timeout")
	fs.DurationVar(&pollInterval, "poll-interval", 5*time.Second, "delay between Mimir validation checks")
	fs.BoolVar(&insecureSkipVerify, "insecure-skip-verify", false, "skip TLS certificate verification")
	fs.BoolVar(&dryRun, "dry-run", false, "validate and report the upload without sending it")
	fs.Var(&extraHeaders, "header", `extra HTTP header "Key: Value" (repeatable)`)
	_ = cmd.MarkFlagRequired("in")
	return cmd
}

type tsdbBlockFile struct {
	RelPath   string `json:"relPath"`
	SizeBytes int64  `json:"sizeBytes"`
}

type tsdbBlockMeta struct {
	ULID  ulid.ULID
	Files []tsdbBlockFile
	JSON  []byte
}

func loadTSDBBlock(path string) (tsdbBlockMeta, error) {
	info, err := os.Stat(path)
	if err != nil {
		return tsdbBlockMeta{}, fmt.Errorf("stat block: %w", err)
	}
	if !info.IsDir() {
		return tsdbBlockMeta{}, fmt.Errorf("block path is not a directory")
	}
	metaPath := filepath.Join(path, "meta.json")
	metaJSON, err := os.ReadFile(metaPath) // #nosec G304 - path is controlled by the user
	if err != nil {
		return tsdbBlockMeta{}, fmt.Errorf("reading meta.json: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(metaJSON, &raw); err != nil {
		return tsdbBlockMeta{}, fmt.Errorf("decoding meta.json: %w", err)
	}
	var idText string
	if err := json.Unmarshal(raw["ulid"], &idText); err != nil {
		return tsdbBlockMeta{}, fmt.Errorf("reading block ULID: %w", err)
	}
	id, err := ulid.Parse(idText)
	if err != nil {
		return tsdbBlockMeta{}, fmt.Errorf("parsing block ULID: %w", err)
	}
	var version int
	if err := json.Unmarshal(raw["version"], &version); err != nil || version != 1 {
		return tsdbBlockMeta{}, fmt.Errorf("only TSDB block version 1 is supported")
	}
	files := []tsdbBlockFile{{RelPath: "meta.json", SizeBytes: int64(len(metaJSON))}}
	for _, relPath := range []string{"index"} {
		if err := appendTSDBFile(path, relPath, &files); err != nil {
			return tsdbBlockMeta{}, err
		}
	}
	err = filepath.WalkDir(filepath.Join(path, "chunks"), func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(path, filePath)
		if err != nil {
			return err
		}
		return appendTSDBFile(path, filepath.ToSlash(relPath), &files)
	})
	if err != nil {
		return tsdbBlockMeta{}, fmt.Errorf("listing chunks: %w", err)
	}
	raw["thanos"], err = json.Marshal(map[string]any{"files": files})
	if err != nil {
		return tsdbBlockMeta{}, fmt.Errorf("encoding file metadata: %w", err)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return tsdbBlockMeta{}, fmt.Errorf("encoding block metadata: %w", err)
	}
	return tsdbBlockMeta{ULID: id, Files: files, JSON: encoded}, nil
}

func appendTSDBFile(blockPath, relPath string, files *[]tsdbBlockFile) error {
	info, err := os.Stat(filepath.Join(blockPath, filepath.FromSlash(relPath)))
	if err != nil {
		return fmt.Errorf("stat %s: %w", relPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("TSDB block file %s is a directory", relPath)
	}
	*files = append(*files, tsdbBlockFile{RelPath: relPath, SizeBytes: info.Size()})
	return nil
}

func runTSDBBackfill(blockPath, endpoint, tenantID, username, password, bearerToken, userAgent string, timeout, pollInterval time.Duration, insecureSkipVerify, dryRun bool, extraHeaders headerList) error {
	block, err := loadTSDBBlock(blockPath)
	if err != nil {
		return err
	}
	if endpoint == "" {
		return fmt.Errorf("--url is required")
	}
	if pollInterval < 0 {
		return fmt.Errorf("poll interval must not be negative")
	}
	baseURL := strings.TrimRight(endpoint, "/") + "/api/v1/upload/block/" + url.PathEscape(block.ULID.String())
	if dryRun {
		fmt.Printf("TSDB block %s: %d files, upload URL %s\n", block.ULID, len(block.Files), baseURL)
		return nil
	}
	client := newTSDBHTTPClient(timeout, insecureSkipVerify)
	headers := tsdbRequestHeaders(tenantID, username, password, bearerToken, userAgent, extraHeaders)
	if _, err := tsdbHTTP(client, http.MethodPost, baseURL+"/start", block.JSON, headers); err != nil {
		return fmt.Errorf("starting block upload: %w", err)
	}
	for _, file := range block.Files {
		if file.RelPath == "meta.json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(blockPath, filepath.FromSlash(file.RelPath))) // #nosec G304 - path is controlled by the user
		if err != nil {
			return fmt.Errorf("reading %s: %w", file.RelPath, err)
		}
		if _, err := tsdbHTTP(client, http.MethodPost, baseURL+"/files?path="+url.QueryEscape(file.RelPath), data, headers); err != nil {
			return fmt.Errorf("uploading %s: %w", file.RelPath, err)
		}
	}
	if _, err := tsdbHTTP(client, http.MethodPost, baseURL+"/finish", nil, headers); err != nil {
		return fmt.Errorf("finishing block upload: %w", err)
	}
	for {
		body, err := tsdbHTTP(client, http.MethodGet, baseURL+"/check", nil, headers)
		if err != nil {
			return fmt.Errorf("checking block upload: %w", err)
		}
		var result struct {
			State string `json:"result"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return fmt.Errorf("decoding block upload status: %w", err)
		}
		switch result.State {
		case "complete":
			return nil
		case "failed":
			return fmt.Errorf("Mimir rejected block: %s", result.Error)
		case "":
			return fmt.Errorf("Mimir returned an empty block upload status")
		}
		if pollInterval > 0 {
			time.Sleep(pollInterval)
		}
	}
}

func newTSDBHTTPClient(timeout time.Duration, insecureSkipVerify bool) *http.Client {
	client := &http.Client{Timeout: timeout}
	if insecureSkipVerify {
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // opt-in via --insecure-skip-verify
	}
	return client
}

func tsdbRequestHeaders(tenantID, username, password, bearerToken, userAgent string, extra headerList) http.Header {
	headers := make(http.Header)
	headers.Set("User-Agent", userAgent)
	if tenantID != "" {
		headers.Set("X-Scope-OrgID", tenantID)
	}
	if bearerToken != "" {
		headers.Set("Authorization", "Bearer "+bearerToken)
	} else if username != "" {
		headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(username+":"+password)))
	}
	for _, header := range extra {
		if key, value, ok := strings.Cut(header, ":"); ok {
			headers.Set(strings.TrimSpace(key), strings.TrimSpace(value))
		}
	}
	return headers
}

func tsdbHTTP(client *http.Client, method, requestURL string, body []byte, headers http.Header) ([]byte, error) {
	req, err := http.NewRequest(method, requestURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header = headers.Clone()
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("Mimir returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}
	return responseBody, nil
}

func runTSDB(inPath, outPath string, from inputFormat, pbFmt pbFormat, blockDuration time.Duration) error {
	if blockDuration <= 0 {
		return fmt.Errorf("block duration must be positive")
	}
	blockRange := int64(blockDuration / time.Millisecond)
	if blockRange <= 0 {
		return fmt.Errorf("block duration must be at least 1ms")
	}
	families, err := loadFamilies(inPath, from, pbFmt)
	if err != nil {
		return err
	}
	request := buildWriteRequest(families, time.Now().UnixMilli())
	if len(request.Timeseries) == 0 {
		return fmt.Errorf("input contains no time series")
	}
	if err := os.MkdirAll(outPath, 0o755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}
	writer, err := tsdb.NewBlockWriter(promslog.NewNopLogger(), outPath, blockRange)
	if err != nil {
		return fmt.Errorf("creating TSDB block writer: %w", err)
	}
	defer func() { _ = writer.Close() }()
	ctx := context.Background()
	app := writer.Appender(ctx)
	for _, series := range request.Timeseries {
		lset := promLabels(series.Labels)
		var ref storage.SeriesRef
		for _, sample := range series.Samples {
			ref, err = app.Append(ref, lset, sample.Timestamp, sample.Value)
			if err != nil {
				return fmt.Errorf("appending series: %w", err)
			}
		}
	}
	if err := app.Commit(); err != nil {
		return fmt.Errorf("committing samples: %w", err)
	}
	if _, err := writer.Flush(ctx); err != nil {
		return fmt.Errorf("flushing TSDB block: %w", err)
	}
	return nil
}

func promLabels(src []prompb.Label) labels.Labels {
	dst := make([]labels.Label, 0, len(src))
	for _, label := range src {
		dst = append(dst, labels.Label{Name: label.Name, Value: label.Value})
	}
	return labels.New(dst...)
}
