// Package cli builds the eucd command tree.
//
// Two rules hold throughout, because a command-line tool that breaks them cannot be used in
// a script or a CI step:
//
// Exit codes mean something. 0 is success; each deliberate failure has its own code, listed
// in the README and defined in [pkg/errors]. A command never exits 0 after failing.
//
// Nothing is skipped silently. Warnings are printed; partial results say they are partial.
// A run that was capped by a limit reports that it was capped, so a short result is never
// mistaken for a complete one.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	eucd "github.com/iobyte-labs/eu-company-discovery"
	eucderrors "github.com/iobyte-labs/eu-company-discovery/pkg/errors"
)

// globals are the flags every command shares.
type globals struct {
	configDir string
	dbPath    string
	verbose   bool
	jsonOut   bool
}

// Execute runs the command tree and returns the process exit code.
//
// Signals are turned into a cancelled context rather than an immediate exit, so a crawl
// interrupted with Ctrl-C finishes its current write and leaves a consistent database.
func Execute(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := &globals{}
	root := newRootCmd(opts)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SilenceUsage = true
	root.SilenceErrors = true

	if err := root.ExecuteContext(ctx); err != nil {
		// A cancelled context is the operator pressing Ctrl-C, not a failure to report as
		// one, but it must still not exit 0.
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "interrupted; nothing further was written")
			return 130
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitCodeFor(err)
	}
	return 0
}

func exitCodeFor(err error) int {
	var typed *eucderrors.Error
	if errors.As(err, &typed) {
		return typed.ExitCode()
	}
	return 1
}

func newRootCmd(opts *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "eucd",
		Short: "Discover companies through industry association directories",
		Long: "eucd discovers companies through industry association directories, resolves " +
			"their official websites, extracts published recruitment contacts, and ranks them " +
			"against a configurable profile.\n\n" +
			"It writes no applications and sends no mail. That belongs to a private consumer " +
			"of the exported data.",
		SilenceUsage:      true,
		DisableAutoGenTag: true,
	}

	flags := root.PersistentFlags()
	flags.StringVar(&opts.configDir, "config-dir", envOr("EUCD_CONFIG_DIR", "config"),
		"directory holding countries/, sectors/ and associations/ (env EUCD_CONFIG_DIR)")
	flags.StringVar(&opts.dbPath, "db", envOr("EUCD_DB", "eucd.db"),
		"path to the SQLite database (env EUCD_DB)")
	flags.BoolVarP(&opts.verbose, "verbose", "v", false, "log progress to stderr")
	flags.BoolVar(&opts.jsonOut, "json", false, "emit machine-readable JSON where supported")

	root.AddCommand(
		newVersionCmd(),
		newConfigCmd(opts),
		newAdaptersCmd(opts),
		newAssociationsCmd(opts),
		newDBCmd(opts),
		newDiscoverCmd(opts),
		newCompaniesCmd(opts),
		newExportCmd(opts),
		newEnrichCmd(opts),
		newClassifyCmd(opts),
		newImportCmd(opts),
		newRankCmd(opts),
	)
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the engine and data-contract versions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "eucd %s\nexport schema %s\n",
				eucd.Version, eucd.ExportSchemaVersion)
			return nil
		},
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
