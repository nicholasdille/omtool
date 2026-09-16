package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/tsdb"
)

func TestRunTSDBOpenMetrics(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "metrics.om")
	output := filepath.Join(dir, "blocks")
	if err := os.WriteFile(input, []byte("# TYPE requests_total counter\nrequests_total{method=\"GET\"} 3 1700000000000\n# EOF\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Mkdir(output, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := runTSDB(input, output, inOpenMetrics, pbBinary, time.Hour); err != nil {
		t.Fatalf("runTSDB: %v", err)
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("expected one TSDB block directory, got %v", entries)
	}
	block, err := tsdb.OpenBlock(nil, filepath.Join(output, entries[0].Name()), nil, nil)
	if err != nil {
		t.Fatalf("OpenBlock: %v", err)
	}
	if err := block.Close(); err != nil {
		t.Fatalf("Close block: %v", err)
	}
}

func TestRunTSDBRejectsInvalidDuration(t *testing.T) {
	err := runTSDB("does-not-matter", t.TempDir(), inOpenMetrics, pbBinary, 0)
	if err == nil {
		t.Fatal("expected invalid duration error")
	}
}

func TestNewTSDBCmdFlags(t *testing.T) {
	cmd := newTSDBCmd()
	if cmd.Use != "tsdb" {
		t.Errorf("Use = %q, want tsdb", cmd.Use)
	}
	if cmd.Flags().Lookup("in") != nil {
		t.Error("tsdb parent should not define --in")
	}
	create, _, err := cmd.Find([]string{"create"})
	if err != nil {
		t.Fatalf("Find create: %v", err)
	}
	if create == nil || create.Use != "create" {
		t.Fatalf("expected create subcommand, got %v", create)
	}
	for _, flag := range []string{"in", "out", "from", "pb-format", "block-duration"} {
		if create.Flags().Lookup(flag) == nil {
			t.Errorf("expected --%s flag", flag)
		}
	}
	backfill, _, err := cmd.Find([]string{"backfill"})
	if err != nil {
		t.Fatalf("Find backfill: %v", err)
	}
	if backfill == nil || backfill.Use != "backfill" {
		t.Fatalf("expected backfill subcommand, got %v", backfill)
	}
}

func makeTestBlock(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "01J00000000000000000000000")
	if err := os.MkdirAll(filepath.Join(dir, "chunks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"ulid":"01J00000000000000000000000","version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"index", "chunks/000001"} {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRunTSDBSend(t *testing.T) {
	blockDir := makeTestBlock(t)
	block, err := loadTSDBBlock(blockDir)
	if err != nil {
		t.Fatalf("loadTSDBBlock: %v", err)
	}
	if len(block.Files) != 3 {
		t.Fatalf("block files = %+v", block.Files)
	}
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/start") {
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode start body: %v", err)
			}
			if len(body["thanos"]) == 0 {
				t.Error("start body missing thanos file metadata")
			}
		}
		if strings.HasSuffix(r.URL.Path, "/check") {
			_, _ = fmt.Fprint(w, `{"result":"complete"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if err := runTSDBBackfill(blockDir, server.URL, "tenant", "", "", "", "test", time.Second, 0, false, false, nil); err != nil {
		t.Fatalf("runTSDBBackfill: %v", err)
	}
	if len(requests) != 5 {
		t.Fatalf("requests = %v, want start, 2 files, finish and check", requests)
	}
}

func TestLoadTSDBBlockErrors(t *testing.T) {
	tests := []struct {
		name string
		edit func(string) error
	}{
		{"missing path", func(string) error { return nil }},
		{"missing meta", func(dir string) error { return os.Remove(filepath.Join(dir, "meta.json")) }},
		{"invalid meta json", func(dir string) error { return os.WriteFile(filepath.Join(dir, "meta.json"), []byte("{"), 0o600) }},
		{"missing ulid", func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"version":1}`), 0o600)
		}},
		{"invalid ulid", func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"ulid":"bad","version":1}`), 0o600)
		}},
		{"invalid version", func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"ulid":"01J00000000000000000000000","version":2}`), 0o600)
		}},
		{"missing index", func(dir string) error { return os.Remove(filepath.Join(dir, "index")) }},
		{"missing chunks", func(dir string) error { return os.RemoveAll(filepath.Join(dir, "chunks")) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "missing path" {
				if _, err := loadTSDBBlock(filepath.Join(t.TempDir(), "missing")); err == nil {
					t.Fatal("expected error")
				}
				return
			}
			dir := makeTestBlock(t)
			if err := tt.edit(dir); err != nil {
				t.Fatal(err)
			}
			if _, err := loadTSDBBlock(dir); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestTSDBRequestHeaders(t *testing.T) {
	headers := tsdbRequestHeaders("tenant", "user", "pass", "", "ua", headerList{"X-Test: value"})
	if headers.Get("X-Scope-OrgID") != "tenant" || headers.Get("User-Agent") != "ua" || headers.Get("X-Test") != "value" {
		t.Fatalf("unexpected headers: %v", headers)
	}
	if headers.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("user:pass")) {
		t.Errorf("unexpected basic auth: %q", headers.Get("Authorization"))
	}
	headers = tsdbRequestHeaders("", "user", "pass", "token", "ua", headerList{"Authorization: override"})
	if headers.Get("Authorization") != "override" {
		t.Errorf("extra header should override auth, got %q", headers.Get("Authorization"))
	}
}

func TestTSDBHTTPErrorPaths(t *testing.T) {
	if _, err := tsdbHTTP(&http.Client{}, http.MethodGet, "://bad", nil, nil); err == nil {
		t.Fatal("expected invalid URL error")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, "bad block")
	}))
	defer server.Close()
	if _, err := tsdbHTTP(server.Client(), http.MethodGet, server.URL, nil, nil); err == nil {
		t.Fatal("expected non-2xx error")
	}
	if client := newTSDBHTTPClient(time.Second, true); client.Transport == nil {
		t.Fatal("expected custom transport for insecure TLS mode")
	}
}

func TestRunTSDBSendValidationAndStates(t *testing.T) {
	blockDir := makeTestBlock(t)
	for _, test := range []struct {
		name, status string
	}{
		{"failed", `{"result":"failed","error":"invalid"}`},
		{"empty", `{}`},
		{"invalid json", `{`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/check") {
					_, _ = fmt.Fprint(w, test.status)
				}
			}))
			defer server.Close()
			if err := runTSDBBackfill(blockDir, server.URL, "", "", "", "", "", time.Second, 0, false, false, nil); err == nil {
				t.Fatal("expected upload status error")
			}
		})
	}
	if err := runTSDBBackfill(blockDir, "", "", "", "", "", "", time.Second, 0, false, false, nil); err == nil {
		t.Fatal("expected missing URL error")
	}
	if err := runTSDBBackfill(blockDir, "http://localhost", "", "", "", "", "", time.Second, -time.Second, false, true, nil); err == nil {
		t.Fatal("expected negative poll interval error")
	}
}

func TestRunTSDBSendRequestFailuresAndPending(t *testing.T) {
	blockDir := makeTestBlock(t)
	for _, failPath := range []string{"/start", "/finish"} {
		t.Run(failPath, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, failPath) {
					http.Error(w, "failed", http.StatusInternalServerError)
				}
			}))
			defer server.Close()
			if err := runTSDBBackfill(blockDir, server.URL, "", "", "", "", "", time.Second, 0, false, false, nil); err == nil {
				t.Fatal("expected request failure")
			}
		})
	}

	checks := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/check") {
			checks++
			if checks == 1 {
				_, _ = fmt.Fprint(w, `{"result":"pending"}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"result":"complete"}`)
		}
	}))
	defer server.Close()
	if err := runTSDBBackfill(blockDir, server.URL, "", "", "", "", "", time.Second, time.Millisecond, false, false, nil); err != nil {
		t.Fatalf("pending upload: %v", err)
	}
	if checks != 2 {
		t.Fatalf("checks = %d, want 2", checks)
	}
}

func TestRunTSDBErrors(t *testing.T) {
	if err := runTSDB("/missing", filepath.Join(t.TempDir(), "out"), inOpenMetrics, pbBinary, time.Second); err == nil {
		t.Fatal("expected missing input error")
	}
	input := filepath.Join(t.TempDir(), "empty.om")
	if err := os.WriteFile(input, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runTSDB(input, filepath.Join(t.TempDir(), "out"), inOpenMetrics, pbBinary, time.Second); err == nil {
		t.Fatal("expected empty input error")
	}
	outFile := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(outFile, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	input = filepath.Join(t.TempDir(), "metric.om")
	if err := os.WriteFile(input, []byte("# TYPE foo gauge\nfoo 1\n# EOF\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runTSDB(input, outFile, inOpenMetrics, pbBinary, time.Second); err == nil {
		t.Fatal("expected output directory error")
	}
}
