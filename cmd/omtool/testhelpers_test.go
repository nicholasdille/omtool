package main

import (
	"errors"
	"os"
)

// writeTempFile is a small test helper for writing fixture files.
func writeTempFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// readTempFile is a small test helper for reading files written by the
// code under test.
func readTempFile(path string) (string, error) {
	b, err := os.ReadFile(path) // #nosec G304 - this is a test in a temp dir
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// failAfterWriter is an io.Writer test double that succeeds for the first
// n writes and then fails every write after that, used to exercise error
// handling paths around repeated Fprintf/Write calls.
type failAfterWriter struct {
	n int
}

var errFailAfterWriter = errors.New("failAfterWriter: simulated write failure")

func (f *failAfterWriter) Write(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errFailAfterWriter
	}
	f.n--
	return len(p), nil
}
