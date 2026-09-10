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
	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// newRootCmd builds omtool's root command and wires up its "convert" and
// "send" subcommands.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "omtool",
		Short:         "Convert and send Prometheus/OpenMetrics metric data",
		Long:          `omtool converts Prometheus/OpenMetrics metric data between formats.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newConvertCmd(), newSendCmd())
	return root
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
