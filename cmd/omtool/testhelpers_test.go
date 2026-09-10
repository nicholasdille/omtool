package main

import "os"

// writeTempFile is a small test helper for writing fixture files.
func writeTempFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// readTempFile is a small test helper for reading files written by the
// code under test.
func readTempFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
