package main

// This file implements the "validate" subcommand: checking that an input
// file conforms to the OpenMetrics text exposition format, reporting a
// parse error (with as much detail as the parser gives us) if it does not.

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// newValidateCmd builds the "validate" subcommand: it checks that an input
// file is well-formed OpenMetrics text exposition format.
func newValidateCmd() *cobra.Command {
	var (
		inPath string
		quiet  bool
	)

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate that an input file is well-formed OpenMetrics text",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runValidate(cmd.OutOrStdout(), inPath, quiet); err != nil {
				return fmt.Errorf("omtool validate: %w", err)
			}
			return nil
		},
	}

	fs := cmd.Flags()
	fs.StringVar(&inPath, "in", "", `input file ("-" for stdin)`)
	fs.BoolVar(&quiet, "quiet", false, "suppress the success summary; print nothing on success")
	_ = cmd.MarkFlagRequired("in")

	return cmd
}

// runValidate reads inPath (or stdin) and parses it strictly as
// OpenMetrics text exposition format, per parseOpenMetrics (which enforces
// the format's rules, including the mandatory trailing "# EOF" marker).
// It reports the number of metric families and series found on success.
func runValidate(w io.Writer, inPath string, quiet bool) error {
	in, err := openInput(inPath)
	if err != nil {
		return fmt.Errorf("opening input: %w", err)
	}
	defer func() { _ = in.Close() }()

	data, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("reading input: %w", err)
	}

	families, err := parseOpenMetrics(data)
	if err != nil {
		return fmt.Errorf("invalid OpenMetrics input: %w", err)
	}

	if quiet {
		return nil
	}

	nSeries := 0
	for _, mf := range families {
		nSeries += len(mf.GetMetric())
	}
	_, err = fmt.Fprintf(w, "OK: valid OpenMetrics format (%d metric families, %d series)\n", len(families), nSeries)
	return err
}
