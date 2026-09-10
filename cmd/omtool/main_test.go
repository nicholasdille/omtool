package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestNewRootCmdWiring(t *testing.T) {
	root := newRootCmd()
	if root.Use != "omtool" {
		t.Errorf("Use = %q, want omtool", root.Use)
	}
	names := map[string]bool{}
	for _, c := range root.Commands() {
		names[c.Name()] = true
	}
	if !names["convert"] || !names["send"] {
		t.Errorf("expected convert and send subcommands, got %v", names)
	}
}

func TestBuildVersionReturnsNonEmpty(t *testing.T) {
	if v := buildVersion(); v == "" {
		t.Error("buildVersion() returned empty string")
	}
}

func TestOpenInputStdin(t *testing.T) {
	for _, p := range []string{"-", ""} {
		rc, err := openInput(p)
		if err != nil {
			t.Fatalf("openInput(%q): %v", p, err)
		}
		if rc == nil {
			t.Fatalf("openInput(%q) returned nil ReadCloser", p)
		}
		_ = rc.Close()
	}
}

func TestOpenInputFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/in.txt"
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rc, err := openInput(path)
	if err != nil {
		t.Fatalf("openInput: %v", err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("data = %q, want hello", data)
	}
}

func TestOpenInputMissingFile(t *testing.T) {
	if _, err := openInput("/nonexistent/path/does-not-exist"); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestOpenOutputFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/out.txt"
	wc, err := openOutput(path)
	if err != nil {
		t.Fatalf("openOutput: %v", err)
	}
	if _, err := wc.Write([]byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "data" {
		t.Errorf("got %q, want data", got)
	}
}

func TestNopWriteCloser(t *testing.T) {
	var buf strings.Builder
	wc := nopWriteCloser{&buf}
	if _, err := wc.Write([]byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wc.Close(); err != nil {
		t.Errorf("Close returned error: %v", err)
	}
	if buf.String() != "x" {
		t.Errorf("buf = %q, want x", buf.String())
	}
}

func TestLoadFamiliesFromFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/in.om"
	if err := os.WriteFile(path, []byte("# TYPE foo gauge\nfoo 3\n# EOF\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	families, err := loadFamilies(path, inOpenMetrics, pbBinary)
	if err != nil {
		t.Fatalf("loadFamilies: %v", err)
	}
	if len(families) != 1 || families[0].GetName() != "foo" {
		t.Fatalf("unexpected families: %+v", families)
	}
}

func TestLoadFamiliesMissingFile(t *testing.T) {
	if _, err := loadFamilies("/nonexistent/path", inOpenMetrics, pbBinary); err == nil {
		t.Error("expected error for missing input file")
	}
}
