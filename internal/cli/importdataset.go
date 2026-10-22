package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	eucderrors "github.com/iobyte-labs/eu-company-discovery/pkg/errors"
	"github.com/iobyte-labs/eu-company-discovery/pkg/export"
)

func newImportDatasetCmd(opts *globals) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "dataset <companies.json>",
		Short: "Rebuild the database from this engine's own JSON export",
		Long: `Reads a file written by "eucd export --format json" and writes it back into a
database, preserving every company id.

This is how a scheduled run keeps its memory without committing a binary database to a
repository: the exported JSON is the durable state, and the database is rebuilt from it
whenever it is missing. Restoring a dataset is idempotent — restoring the same file twice
leaves the same rows.

It preserves company ids rather than deriving them, because a company's id comes from the
first identity it was ever seen under and that cannot be recomputed from the finished row.
Crawl state is preserved too: without it the next run would treat every company as unvisited
and crawl the whole dataset again.

It refuses, rather than repairs, a file it cannot restore faithfully — two companies sharing
a domain, a duplicate id, a missing first_seen_at. Each of those would otherwise produce a
database that looks correct and is quietly wrong.

Only use it for this engine's own export. Data from anywhere else has no id of ours; import
it with "import legacy-csv" or discover it, so that identity is resolved properly.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			file, err := os.Open(args[0])
			if err != nil {
				return eucderrors.Wrap(eucderrors.KindUsage, err, "open %q", args[0])
			}
			defer func() { _ = file.Close() }()

			envelope, err := export.Read(file)
			if err != nil {
				return eucderrors.Wrap(eucderrors.KindUsage, err,
					"%q is not a dataset this engine can read", args[0])
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s: schema %s, %d company/companies, generated %s by %s\n",
				args[0], envelope.SchemaVersion, len(envelope.Companies),
				envelope.GeneratedAt.Format("2006-01-02 15:04 UTC"), envelope.Generator)

			if dryRun {
				fmt.Fprintln(out, "dry run: nothing was written")
				return nil
			}

			db, err := openStore(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer db.Close()

			result, err := db.RestoreCompanies(cmd.Context(), envelope.Companies)
			if err != nil {
				return eucderrors.Wrap(eucderrors.KindUsage, err, "restore %q", args[0])
			}
			fmt.Fprintf(out, "Restored %d company/companies into %s: %d contact(s), "+
				"%d source(s), %d identity key(s).\n",
				result.Companies, db.Path(), result.Contacts, result.Sources, result.Identities)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "read and check the file, write nothing")
	return cmd
}
