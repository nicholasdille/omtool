package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestRunValidateValidInput(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/valid.om"
	data := "# HELP requests_total total requests\n# TYPE requests_total counter\nrequests_total{method=\"get\"} 10\n# EOF\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var buf bytes.Buffer
	if err := runValidate(&buf, path, false); err != nil {
		t.Fatalf("runValidate: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "OK") || !strings.Contains(got, "1 metric families") || !strings.Contains(got, "1 series") {
		t.Errorf("output = %q, want summary mentioning OK, 1 metric families and 1 series", got)
	}
}

func TestRunValidateQuietSuppressesOutput(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/valid.om"
	data := "# TYPE foo gauge\nfoo 3\n# EOF\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var buf bytes.Buffer
	if err := runValidate(&buf, path, true); err != nil {
		t.Fatalf("runValidate: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output with quiet=true, got %q", buf.String())
	}
}

func TestRunValidateMissingEOFMarker(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/invalid.om"
	data := "# TYPE foo gauge\nfoo 3\n" // missing trailing "# EOF"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var buf bytes.Buffer
	err := runValidate(&buf, path, false)
	if err == nil {
		t.Fatal("expected error for input missing trailing # EOF marker")
	}
	if !strings.Contains(err.Error(), "EOF") {
		t.Errorf("error = %v, want it to mention the missing EOF marker", err)
	}
}

func TestRunValidateMalformedInput(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/malformed.om"
	data := "not openmetrics at all {{{\n# EOF\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var buf bytes.Buffer
	if err := runValidate(&buf, path, false); err == nil {
		t.Fatal("expected error for malformed input")
	}
}

func TestRunValidateMissingFile(t *testing.T) {
	var buf bytes.Buffer
	if err := runValidate(&buf, "/nonexistent/path/does-not-exist", false); err == nil {
		t.Error("expected error for missing input file")
	}
}

func TestNewValidateCmdRequiresIn(t *testing.T) {
	cmd := newValidateCmd()
	cmd.SetArgs([]string{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when --in is not provided")
	}
}
