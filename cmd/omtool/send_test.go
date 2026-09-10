package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestHeaderList(t *testing.T) {
	var h headerList
	if err := h.Set("X-Foo: bar"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := h.Set("X-Baz: qux"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if h.Type() != "stringArray" {
		t.Errorf("Type() = %q", h.Type())
	}
	want := "X-Foo: bar,X-Baz: qux"
	if h.String() != want {
		t.Errorf("String() = %q, want %q", h.String(), want)
	}
}

func TestSendRequestHeadersAndAuth(t *testing.T) {
	var gotHeaders http.Header
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := sendRequest(srv.URL, []byte("payload"), "tenant1", "", "", "tok123", "omtool-test/1.0", 5*time.Second, false, headerList{"X-Extra: v1"})
	if err != nil {
		t.Fatalf("sendRequest: %v", err)
	}
	if string(gotBody) != "payload" {
		t.Errorf("body = %q, want payload", gotBody)
	}
	if gotHeaders.Get("Content-Encoding") != "snappy" {
		t.Errorf("Content-Encoding = %q", gotHeaders.Get("Content-Encoding"))
	}
	if gotHeaders.Get("Content-Type") != "application/x-protobuf" {
		t.Errorf("Content-Type = %q", gotHeaders.Get("Content-Type"))
	}
	if gotHeaders.Get("X-Prometheus-Remote-Write-Version") != remoteWriteVersion {
		t.Errorf("remote-write version header = %q", gotHeaders.Get("X-Prometheus-Remote-Write-Version"))
	}
	if gotHeaders.Get("X-Scope-OrgID") != "tenant1" {
		t.Errorf("X-Scope-OrgID = %q", gotHeaders.Get("X-Scope-OrgID"))
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

func TestSendRequestBasicAuth(t *testing.T) {
	var gotUser, gotPass string
	var gotOK bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotOK = r.BasicAuth()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := sendRequest(srv.URL, []byte("x"), "", "alice", "secret", "", "ua", 5*time.Second, false, nil)
	if err != nil {
		t.Fatalf("sendRequest: %v", err)
	}
	if !gotOK || gotUser != "alice" || gotPass != "secret" {
		t.Errorf("basic auth = (%q, %q, %v), want (alice, secret, true)", gotUser, gotPass, gotOK)
	}
}

func TestSendRequestNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	err := sendRequest(srv.URL, []byte("x"), "", "", "", "", "ua", 5*time.Second, false, nil)
	if err == nil {
		t.Fatal("expected error for non-2xx response")
	}
}

func TestSendRequestInvalidHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := sendRequest(srv.URL, []byte("x"), "", "", "", "", "ua", 5*time.Second, false, headerList{"no-colon-here"})
	if err == nil {
		t.Fatal("expected error for malformed header")
	}
}

func TestRunSendUnknownTarget(t *testing.T) {
	err := runSend("in", inAuto, pbBinary, "", "bogus-target", "", "", "", "", "ua", time.Second, false, true, nil)
	if err == nil {
		t.Fatal("expected error for unknown target")
	}
}

func TestRunSendMissingURLWithoutDryRun(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/in.om"
	if err := os.WriteFile(path, []byte("# TYPE foo gauge\nfoo 1\n# EOF\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err := runSend(path, inOpenMetrics, pbBinary, "", "prometheus", "", "", "", "", "ua", time.Second, false, false, nil)
	if err == nil {
		t.Fatal("expected error when --url is empty and --dry-run is false")
	}
}

func TestRunSendDryRun(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/in.om"
	if err := os.WriteFile(path, []byte("# TYPE foo gauge\nfoo 1\n# EOF\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err := runSend(path, inOpenMetrics, pbBinary, "", "prometheus", "", "", "", "", "ua", time.Second, false, true, nil)
	if err != nil {
		t.Fatalf("runSend dry-run: %v", err)
	}
}

func TestRunSendNoFamiliesDecoded(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/in.om"
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err := runSend(path, inOpenMetrics, pbBinary, "", "prometheus", "", "", "", "", "ua", time.Second, false, true, nil)
	if err == nil {
		t.Fatal("expected error when no metric families are decoded")
	}
}

func TestNewSendCmdWiring(t *testing.T) {
	cmd := newSendCmd()
	if cmd.Use != "send" {
		t.Errorf("Use = %q, want send", cmd.Use)
	}
	if cmd.Flags().Lookup("in") == nil {
		t.Error("expected --in flag")
	}
	if cmd.Flags().Lookup("url") == nil {
		t.Error("expected --url flag")
	}
}
