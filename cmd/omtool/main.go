// Command omtool converts Prometheus/OpenMetrics metric data between
// OpenMetrics text exposition format, Prometheus protobuf MetricFamily
// messages (io.prometheus.client.MetricFamily, aka client_model), and
// human readable text, and can send metric data to a Prometheus/Mimir
// remote-write endpoint.
//
// It merges three previous single-purpose CLIs, openmetrics2proto,
// proto2text and remotewrite, into one tool whose "convert" subcommand
// auto-detects (or is told) its input format and can target any of the
// supported output formats, and whose "send" subcommand ships metric data
// to a remote-write endpoint.
package main

import (
	"fmt"
	"io"
	"os"

	dto "github.com/prometheus/client_model/go"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "convert":
		runConvertCmd(args)
	case "send":
		runSendCmd(args)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "omtool: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `omtool converts Prometheus/OpenMetrics metric data between formats.

Usage:

	omtool <command> [arguments]

Commands:

	convert    convert metric data between OpenMetrics, protobuf and human readable formats
	send       send metric data to a Prometheus/Mimir remote-write endpoint

Use "omtool convert -h" or "omtool send -h" for details on each command's flags.`)
}

func openInput(path string) (io.ReadCloser, error) {
	if path == "-" || path == "" {
		return io.NopCloser(os.Stdin), nil
	}
	return os.Open(path)
}

// loadFamilies reads all of inPath (or stdin) and decodes it into
// MetricFamily messages per the given input format/protobuf framing.
// Shared by the "convert" and "send" subcommands, which both start by
// turning their input into decoded families before doing their own thing
// with them.
func loadFamilies(inPath string, from inputFormat, pbFmt pbFormat) ([]*dto.MetricFamily, error) {
	in, err := openInput(inPath)
	if err != nil {
		return nil, fmt.Errorf("opening input: %w", err)
	}
	defer in.Close()

	data, err := io.ReadAll(in)
	if err != nil {
		return nil, fmt.Errorf("reading input: %w", err)
	}

	return readFamilies(data, from, pbFmt)
}

func openOutput(path string) (io.WriteCloser, error) {
	if path == "-" || path == "" {
		return nopWriteCloser{os.Stdout}, nil
	}
	return os.Create(path)
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
