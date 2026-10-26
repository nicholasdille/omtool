package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScrapeRequestHeadersAndAuth(t *testing.T) {
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("# EOF\n"))
	}))
	defer srv.Close()

	_, _, err := scrapeRequest(srv.URL, scrapeAcceptHeader, "", "", "tok123", "omtool-test/1.0", 5*time.Second, false, headerList{"X-Extra: v1"})
	if err != nil {
		t.Fatalf("scrapeRequest: %v", err)
	}
	if gotHeaders.Get("Accept") != scrapeAcceptHeader {
		t.Errorf("Accept = %q", gotHeaders.Get("Accept"))
	}
	if gotHeaders.Get("Authorization") != "Bearer tok123" {
		t.Errorf("Authorization = %q", gotHeaders.Get("Authorization"))
	}
	if gotHeaders.Get("User-Agent") != "omtool-test/1.0" {
		t.Errorf("User-Agent = %q", gotHeaders.Get("User-Agent"))
	}
	if gotHeaders.Get("X-Extra") != "v1" {
		t.Errorf("X-Extra = %q", gotHeaders.Get("X-Extra"))
	}
}

func TestScrapeRequestBasicAuth(t *testing.T) {
	var gotUser, gotPass string
	var gotOK bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotOK = r.BasicAuth()
		_, _ = w.Write([]byte("# EOF\n"))
	}))
	defer srv.Close()

	_, _, err := scrapeRequest(srv.URL, scrapeAcceptHeader, "alice", "secret", "", "ua", 5*time.Second, false, nil)
	if err != nil {
		t.Fatalf("scrapeRequest: %v", err)
	}
	if !gotOK || gotUser != "alice" || gotPass != "secret" {
		t.Errorf("basic auth = (%q, %q, %v), want (alice, secret, true)", gotUser, gotPass, gotOK)
	}
}

func TestScrapeRequestExtraHeaderOverridesAccept(t *testing.T) {
	var gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		_, _ = w.Write([]byte("# EOF\n"))
	}))
	defer srv.Close()

	_, _, err := scrapeRequest(srv.URL, scrapeAcceptHeader, "", "", "", "ua", 5*time.Second, false, headerList{"Accept: text/plain"})
	if err != nil {
		t.Fatalf("scrapeRequest: %v", err)
	}
	if gotAccept != "text/plain" {
		t.Errorf("Accept = %q, want text/plain", gotAccept)
	}
}

func TestScrapeRequestInvalidHeader(t *testing.T) {
	_, _, err := scrapeRequest("http://localhost:1/metrics", scrapeAcceptHeader, "", "", "", "ua", 5*time.Second, false, headerList{"no-colon-here"})
	if err == nil {
		t.Fatal("expected error for malformed header")
	}
}

func TestScrapeRequestNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	_, _, err := scrapeRequest(srv.URL, scrapeAcceptHeader, "", "", "", "ua", 5*time.Second, false, nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error with body excerpt for non-2xx response, got %v", err)
	}
}

func TestScrapeMediaType(t *testing.T) {
	openMetricsBody := []byte("# TYPE foo gauge\nfoo 1\n# EOF\n")
	classicBody := []byte("# TYPE foo gauge\nfoo 1\n")
	for name, tc := range map[string]struct {
		contentType string
		body        []byte
		want        string
	}{
		"openmetrics content type":     {"application/openmetrics-text; version=1.0.0", classicBody, openmetricsContentType},
		"openmetrics content type cct": {"APPLICATION/OPENMETRICS-TEXT; charset=utf-8", classicBody, openmetricsContentType},
		"classic content type":         {"text/plain; version=0.0.4; charset=utf-8", openMetricsBody, "text/plain"},
		"plain classic content type":   {"text/plain", openMetricsBody, "text/plain"},
		"empty sniffs EOF":             {"", openMetricsBody, openmetricsContentType},
		"empty sniffs classic":         {"", classicBody, "text/plain"},
		"unknown sniffs EOF":           {"application/x-fancy-flavor", openMetricsBody, openmetricsContentType},
		"no content type sniffs eof":   {"text/html", openMetricsBody, openmetricsContentType},
	} {
		if got := scrapeMediaType(tc.contentType, tc.body); got != tc.want {
			t.Errorf("%s: scrapeMediaType(%q, ...) = %q, want %q", name, tc.contentType, got, tc.want)
		}
	}
}

func TestRunScrapeOpenMetricsOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/openmetrics-text; version=1.0.0")
		_, _ = w.Write([]byte("# HELP foo help\n# TYPE foo gauge\nfoo 3\n# EOF\n"))
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.om")
	if err := runScrape(srv.URL, out, outOpenMetrics, pbBinary, "", "", "", "ua", 5*time.Second, false, nil); err != nil {
		t.Fatalf("runScrape: %v", err)
	}
	data, err := os.ReadFile(out) // #nosec G304 - this is a test in a temp dir
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	families, err := parseOpenMetrics(data)
	if err != nil {
		t.Fatalf("re-parsing scrape output: %v\noutput: %q", err, data)
	}
	if len(families) != 1 || families[0].GetName() != "foo" {
		t.Fatalf("unexpected families: %+v", families)
	}
}

func TestRunScrapeClassicTextContentTypeServesNoEOF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte("# TYPE foo gauge\nfoo 3\n"))
	}))
	defer srv.Close()

	if err := runScrape(srv.URL, os.DevNull, outOpenMetrics, pbBinary, "", "", "", "ua", 5*time.Second, false, nil); err != nil {
		t.Fatalf("runScrape: %v", err)
	}
}

func TestRunScrapeUnknownOutputFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# EOF\n"))
	}))
	defer srv.Close()

	err := runScrape(srv.URL, os.DevNull, outputFormat("bogus"), pbBinary, "", "", "", "ua", 5*time.Second, false, nil)
	if err == nil {
		t.Fatal("expected error for unknown output format")
	}
}

func TestRunScrapeProtobufOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# TYPE foo gauge\nfoo 3\n# EOF\n"))
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.pb")
	if err := runScrape(srv.URL, out, outProtobuf, pbDelimited, "", "", "", "ua", 5*time.Second, false, nil); err != nil {
		t.Fatalf("runScrape: %v", err)
	}
	families, err := loadFamilies(out, inProtobuf, pbDelimited)
	if err != nil {
		t.Fatalf("re-parsing scrape protobuf output: %v", err)
	}
	if len(families) != 1 || families[0].GetName() != "foo" {
		t.Fatalf("unexpected families: %+v", families)
	}
}

func TestRunScrapeEmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	}))
	defer srv.Close()

	err := runScrape(srv.URL, os.DevNull, outOpenMetrics, pbBinary, "", "", "", "ua", 5*time.Second, false, nil)
	if err == nil {
		t.Fatal("expected error for empty scrape response")
	}
	if !strings.Contains(err.Error(), "no metric families") {
		t.Errorf("error = %v, want no-metric-families hint", err)
	}
}

func TestRunScrapeOutputError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# TYPE foo gauge\nfoo 3\n# EOF\n"))
	}))
	defer srv.Close()

	// --out inside a missing parent directory: openOutput must fail.
	err := runScrape(srv.URL, filepath.Join(t.TempDir(), "missing", "out.om"), outOpenMetrics, pbBinary, "", "", "", "ua", 5*time.Second, false, nil)
	if err == nil {
		t.Fatal("expected error opening output in missing directory")
	}
	if !strings.Contains(err.Error(), "opening output") {
		t.Errorf("error = %v, want opening-output wrap", err)
	}
}

func TestNewScrapeCmdWiring(t *testing.T) {
	cmd := newScrapeCmd()
	if cmd.Use != "scrape" {
		t.Errorf("Use = %q, want scrape", cmd.Use)
	}
	for _, name := range []string{"url", "out", "to", "pb-format", "username", "password", "bearer-token", "user-agent", "timeout", "insecure-skip-verify", "header"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("expected --%s flag", name)
		}
	}
}

func TestScrapeRunEWrapsError(t *testing.T) {
	cmd := newScrapeCmd()
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error executing scrape without --url")
	}
}
