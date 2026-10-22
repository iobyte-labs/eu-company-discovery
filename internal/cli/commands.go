package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	eucd "github.com/iobyte-labs/eu-company-discovery"
	"github.com/iobyte-labs/eu-company-discovery/pkg/config"
	"github.com/iobyte-labs/eu-company-discovery/pkg/directory"
	eucderrors "github.com/iobyte-labs/eu-company-discovery/pkg/errors"
	"github.com/iobyte-labs/eu-company-discovery/pkg/export"
	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
	"github.com/iobyte-labs/eu-company-discovery/pkg/store"
)

// loadConfig reads configuration and checks it against the registered adapters, so a
// command never operates on configuration it has not validated.
func loadConfig(opts *globals) (*config.Bundle, error) {
	bundle, err := config.Load(opts.configDir, directory.Names())
	if err != nil {
		return nil, eucderrors.Wrap(eucderrors.KindConfig, err, "configuration in %q is not usable",
			opts.configDir)
	}
	return bundle, nil
}

// openStore opens the database and brings the engine schema up to date. Migration is
// unconditional because it is idempotent, which removes a whole class of "forgot to migrate"
// failures.
func openStore(ctx context.Context, opts *globals) (*store.Store, error) {
	db, err := store.Open(ctx, opts.dbPath)
	if err != nil {
		return nil, eucderrors.Wrap(eucderrors.KindGeneral, err, "open database %q", opts.dbPath)
	}
	if err := db.Migrate(ctx, store.EngineMigrations()); err != nil {
		db.Close()
		return nil, eucderrors.Wrap(eucderrors.KindMigration, err, "migrate %q", opts.dbPath)
	}
	return db, nil
}

func newLogger(opts *globals, w io.Writer) *slog.Logger {
	level := slog.LevelWarn
	if opts.verbose {
		level = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// ---------------------------------------------------------------- config

func newConfigCmd(opts *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and validate configuration",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "Check every country, sector and association file",
		Long: "Reports every problem it finds rather than stopping at the first, so a " +
			"configuration can be fixed in one pass. Exits 2 if anything is wrong.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			bundle, err := loadConfig(opts)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Configuration in %s is valid.\n", bundle.Dir)
			fmt.Fprintf(out, "  countries    %d  (%s)\n",
				len(bundle.Countries), strings.Join(bundle.CountryCodes(), ", "))
			fmt.Fprintf(out, "  sectors      %d  (%s)\n",
				len(bundle.Sectors), strings.Join(bundle.SectorSlugs(), ", "))
			enabled := bundle.EnabledAssociations()
			fmt.Fprintf(out, "  associations %d enabled, %d disabled\n",
				len(enabled), len(bundle.Associations)-len(enabled))
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print the resolved configuration as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			bundle, err := loadConfig(opts)
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(map[string]any{
				"dir":          bundle.Dir,
				"countries":    bundle.Countries,
				"sectors":      bundle.Sectors,
				"associations": bundle.Associations,
			})
		},
	})
	return cmd
}

// ---------------------------------------------------------------- adapters

func newAdaptersCmd(opts *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "adapters",
		Short: "List the directory adapters this build can use",
		Long: "Adapters are resolved by name from an association's `adapter:` field. A " +
			"separately released package can add one without this engine changing; see " +
			"docs/adapter-protocol.md.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			names := directory.Names()
			if opts.jsonOut {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(names)
			}
			if len(names) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No adapters are registered.")
				return nil
			}
			for _, name := range names {
				fmt.Fprintln(cmd.OutOrStdout(), name)
			}
			return nil
		},
	}
}

// ---------------------------------------------------------------- associations

func newAssociationsCmd(opts *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "associations", Short: "Inspect configured sources"}
	var includeDisabled bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List configured association directories",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			bundle, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rows := bundle.EnabledAssociations()
			if includeDisabled {
				rows = nil
				for _, slug := range bundle.AssociationSlugs() {
					rows = append(rows, bundle.Associations[slug])
				}
			}
			if opts.jsonOut {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(rows)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "SLUG\tCOUNTRY\tSECTORS\tADAPTER\tACCESS\tSTATUS")
			for _, association := range rows {
				status := "enabled"
				if association.Disabled {
					status = "disabled"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					association.Slug, association.Country,
					strings.Join(association.Sectors, ","), association.Adapter,
					association.AccessMethod, status)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			// A disabled source is a decision someone made; it should not be invisible.
			if !includeDisabled {
				hidden := len(bundle.Associations) - len(rows)
				if hidden > 0 {
					fmt.Fprintf(cmd.OutOrStdout(),
						"\n%d disabled source(s) hidden; --all shows them with their reasons.\n",
						hidden)
				}
			}
			return nil
		},
	}
	list.Flags().BoolVar(&includeDisabled, "all", false, "include disabled sources")
	cmd.AddCommand(list)
	return cmd
}

// ---------------------------------------------------------------- db

func newDBCmd(opts *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "db", Short: "Database maintenance"}

	cmd.AddCommand(&cobra.Command{
		Use:   "migrate",
		Short: "Create or update the schema",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := openStore(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer db.Close()
			applied, err := db.AppliedMigrations(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is up to date (%d migration(s) applied).\n",
				db.Path(), len(applied))
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show row counts and applied migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := openStore(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer db.Close()

			counts, err := db.Counts(cmd.Context())
			if err != nil {
				return err
			}
			applied, err := db.AppliedMigrations(cmd.Context())
			if err != nil {
				return err
			}
			if opts.jsonOut {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(map[string]any{
					"path": db.Path(), "counts": counts, "migrations": applied,
				})
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "database %s\n\n", db.Path())
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			tables := make([]string, 0, len(counts))
			for table := range counts {
				tables = append(tables, table)
			}
			sort.Strings(tables)
			for _, table := range tables {
				fmt.Fprintf(tw, "  %s\t%d\n", table, counts[table])
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintln(out, "\nmigrations:")
			for _, line := range applied {
				fmt.Fprintf(out, "  %s\n", line)
			}
			return nil
		},
	})

	backup := &cobra.Command{
		Use:   "backup <destination.db>",
		Short: "Write a consistent copy of the database",
		Long: "Uses SQLite's VACUUM INTO, which is safe while the engine is running. A plain " +
			"file copy of a live WAL database can be torn; this cannot.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := openStore(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer db.Close()
			if _, err := os.Stat(args[0]); err == nil {
				return eucderrors.New(eucderrors.KindUsage,
					"%s already exists; refusing to overwrite a backup", args[0])
			}
			if err := db.Backup(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Backed up %s to %s\n", db.Path(), args[0])
			return nil
		},
	}
	cmd.AddCommand(backup)
	return cmd
}

// ---------------------------------------------------------------- discover

func newDiscoverCmd(opts *globals) *cobra.Command {
	var (
		slugs   []string
		country string
		dryRun  bool
	)
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Read configured association directories and record the companies found",
		Long: "Runs each selected association's adapter and upserts what it finds. Repeating a " +
			"discovery run does not create duplicates: companies are resolved by registrable " +
			"domain, or by normalised name within one country.\n\n" +
			"--dry-run reads the sources and reports what would be recorded, writing nothing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			bundle, err := loadConfig(opts)
			if err != nil {
				return err
			}
			selected, err := selectAssociations(bundle, slugs, country)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			logger := newLogger(opts, cmd.ErrOrStderr())

			var db *store.Store
			if !dryRun {
				if db, err = openStore(cmd.Context(), opts); err != nil {
					return err
				}
				defer db.Close()
			}

			var totalRecords, totalCreated, totalUpdated, totalWarnings, totalBlocked int
			for _, association := range selected {
				runtime, err := association.Model()
				if err != nil {
					return eucderrors.Wrap(eucderrors.KindConfig, err, "association %q",
						association.Slug)
				}
				adapter, err := directory.Get(runtime.Adapter)
				if err != nil {
					return eucderrors.Wrap(eucderrors.KindAdapterNotFound, err,
						"association %q", association.Slug)
				}

				fmt.Fprintf(out, "\n%s (%s, adapter %s)\n",
					association.Slug, association.Country, association.Adapter)

				result, err := adapter.Discover(cmd.Context(), directory.Request{
					Association: runtime,
					Limits:      runtime.Limits,
					Logger:      logger.With("association", association.Slug),
					// Fetcher is nil: the fetching adapters arrive in Phase 2, and a
					// manual adapter must not be handed one it could misuse.
				})
				if err != nil {
					return eucderrors.Wrap(eucderrors.KindAdapter, err,
						"association %q", association.Slug)
				}

				totalRecords += len(result.Records)
				totalWarnings += len(result.Warnings)
				totalBlocked += len(result.Blocked)

				for _, blocked := range result.Blocked {
					fmt.Fprintf(out, "  BLOCKED %s — %s\n", blocked.URL, blocked.Reason)
				}
				for _, warning := range result.Warnings {
					fmt.Fprintf(out, "  warning: %s\n", warning)
				}
				if result.Truncated {
					fmt.Fprintf(out, "  note: stopped at a configured limit; "+
						"this is a partial read of the source\n")
				}

				if dryRun {
					for _, record := range result.Records {
						fmt.Fprintf(out, "  WOULD RECORD  %-45s %s\n",
							truncateForDisplay(record.RawName, 45), record.Website)
					}
					fmt.Fprintf(out, "  %d record(s) read\n", len(result.Records))
					continue
				}

				created, updated, err := recordAll(cmd.Context(), db, association.Slug, result.Records)
				if err != nil {
					return err
				}
				totalCreated += created
				totalUpdated += updated
				fmt.Fprintf(out, "  %d record(s): %d new, %d updated\n",
					len(result.Records), created, updated)
			}

			fmt.Fprintf(out, "\n%d source(s): %d record(s) read",
				len(selected), totalRecords)
			if dryRun {
				fmt.Fprintf(out, " — dry run, nothing written\n")
			} else {
				fmt.Fprintf(out, ", %d new, %d updated\n", totalCreated, totalUpdated)
			}
			if totalWarnings > 0 || totalBlocked > 0 {
				fmt.Fprintf(out, "%d warning(s), %d blocked source(s)\n", totalWarnings, totalBlocked)
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&slugs, "association", nil,
		"association slug to run; repeatable. Default: every enabled source")
	cmd.Flags().StringVar(&country, "country", "", "only run sources for this country code")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "read the sources but write nothing")
	return cmd
}

func selectAssociations(
	bundle *config.Bundle, slugs []string, country string,
) ([]config.Association, error) {
	if len(slugs) > 0 {
		out := make([]config.Association, 0, len(slugs))
		for _, slug := range slugs {
			association, ok := bundle.Associations[slug]
			if !ok {
				return nil, eucderrors.New(eucderrors.KindUsage,
					"no association %q in %s; `eucd associations list --all` shows them",
					slug, bundle.Dir)
			}
			if association.Disabled {
				return nil, eucderrors.New(eucderrors.KindUsage,
					"association %q is disabled: %s", slug, association.DisabledReason)
			}
			out = append(out, association)
		}
		return out, nil
	}

	var out []config.Association
	for _, association := range bundle.EnabledAssociations() {
		if country != "" && association.Country != strings.ToLower(country) {
			continue
		}
		out = append(out, association)
	}
	if len(out) == 0 {
		return nil, eucderrors.New(eucderrors.KindUsage,
			"no enabled association matches; `eucd associations list --all` shows what is configured")
	}
	return out, nil
}

// recordAll turns directory records into companies. One record at a time, each in its own
// transaction, so a failure halfway leaves everything read so far safely stored.
func recordAll(
	ctx context.Context, db *store.Store, associationSlug string, records []model.DirectoryRecord,
) (created, updated int, err error) {
	for _, record := range records {
		company := model.Company{
			Name:        record.RawName,
			Country:     record.Country,
			Region:      record.Region,
			Website:     record.Website,
			Description: record.Description,
			Sectors:     record.Sectors,
			Keywords:    record.Keywords,
			Quality:     model.QualityLow, // a directory row alone is weak evidence
			Sources: []model.Source{{
				AssociationSlug: associationSlug,
				SourceURL:       record.SourceURL,
				RawName:         record.RawName,
				RetrievedAt:     record.RetrievedAt,
			}},
		}
		result, upsertErr := db.UpsertCompany(ctx, company)
		if upsertErr != nil {
			return created, updated, eucderrors.Wrap(eucderrors.KindGeneral, upsertErr,
				"record %q from %s", record.RawName, associationSlug)
		}
		if result.Created {
			created++
		} else {
			updated++
		}
		if result.Conflict != "" {
			// Printed, never swallowed: an identity collision is a thing a human has to
			// look at.
			fmt.Printf("  identity collision: %s\n", result.Conflict)
		}
	}
	return created, updated, nil
}

// ---------------------------------------------------------------- companies

func newCompaniesCmd(opts *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "companies", Short: "Inspect discovered companies"}
	var (
		country     string
		sector      string
		withContact bool
		limit       int
	)
	list := &cobra.Command{
		Use:   "list",
		Short: "List discovered companies",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := openStore(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer db.Close()

			companies, err := db.ListCompanies(cmd.Context(), store.CompanyFilter{
				Country: country, Sector: sector, WithContact: withContact, Limit: limit,
			})
			if err != nil {
				return err
			}
			if opts.jsonOut {
				return export.Write(cmd.OutOrStdout(), export.Envelope{
					Generator: "eucd " + eucd.Version,
					Filter:    filterDescription(country, sector, withContact),
					Companies: companies,
				})
			}
			if len(companies) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No companies match.")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tCOUNTRY\tDOMAIN\tCONTACT\tKIND\tQUALITY")
			for _, company := range companies {
				contact, kind := "—", "—"
				if best, ok := company.BestContact(); ok {
					contact, kind = best.Address, string(best.Kind)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					company.ID, truncateForDisplay(company.Name, 40), company.Country,
					company.CanonicalDomain, contact, kind, company.Quality)
			}
			return tw.Flush()
		},
	}
	list.Flags().StringVar(&country, "country", "", "filter by country code")
	list.Flags().StringVar(&sector, "sector", "", "filter by sector slug")
	list.Flags().BoolVar(&withContact, "with-contact", false, "only companies with a contact")
	list.Flags().IntVar(&limit, "limit", 50, "maximum rows (0 for no limit)")
	cmd.AddCommand(list)
	return cmd
}

func filterDescription(country, sector string, withContact bool) map[string]string {
	filter := map[string]string{}
	if country != "" {
		filter["country"] = country
	}
	if sector != "" {
		filter["sector"] = sector
	}
	if withContact {
		filter["with_contact"] = "true"
	}
	if len(filter) == 0 {
		return nil
	}
	return filter
}

// ---------------------------------------------------------------- export

func newExportCmd(opts *globals) *cobra.Command {
	var (
		format      string
		outputPath  string
		country     string
		sector      string
		withContact bool
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Write discovered companies as versioned JSON or flat CSV",
		Long: "JSON is the data contract: it carries a schema version, provenance and every " +
			"contact. CSV is a flattened view for review and cannot represent more than one " +
			"contact per company. See docs/data-contract.md.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "json" && format != "csv" {
				return eucderrors.New(eucderrors.KindUsage,
					"--format must be json or csv, not %q", format)
			}
			db, err := openStore(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer db.Close()

			companies, err := db.ListCompanies(cmd.Context(), store.CompanyFilter{
				Country: country, Sector: sector, WithContact: withContact,
			})
			if err != nil {
				return err
			}

			writer := cmd.OutOrStdout()
			var outputFile *os.File
			if outputPath != "" {
				if outputFile, err = os.Create(outputPath); err != nil {
					return eucderrors.Wrap(eucderrors.KindGeneral, err, "create %q", outputPath)
				}
				writer = outputFile
			}

			if format == "csv" {
				err = export.WriteCSV(writer, companies)
			} else {
				err = export.Write(writer, export.Envelope{
					Generator: "eucd " + eucd.Version,
					Filter:    filterDescription(country, sector, withContact),
					Companies: companies,
				})
			}

			// Closing a written file is where its last bytes are flushed, so this error is
			// the difference between a complete export and a truncated one. Dropping it is
			// how an export cut short by a full disk gets reported as a success — and this
			// file is the seam between the public engine and a private consumer, which reads
			// it without any way of knowing it was meant to be longer. The write error wins
			// if there is one, because it says more about what went wrong.
			if outputFile != nil {
				if closeErr := outputFile.Close(); closeErr != nil && err == nil {
					err = closeErr
				}
			}
			if err != nil {
				if outputPath != "" {
					return eucderrors.Wrap(eucderrors.KindGeneral, err,
						"writing %q; it may be incomplete and should not be used", outputPath)
				}
				return err
			}

			if outputPath != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "Wrote %d company/companies to %s (%s).\n",
					len(companies), outputPath, format)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "json", "json or csv")
	cmd.Flags().StringVarP(&outputPath, "out", "o", "", "write to this file instead of stdout")
	cmd.Flags().StringVar(&country, "country", "", "filter by country code")
	cmd.Flags().StringVar(&sector, "sector", "", "filter by sector slug")
	cmd.Flags().BoolVar(&withContact, "with-contact", false, "only companies with a contact")
	return cmd
}

func truncateForDisplay(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	if limit <= 1 {
		return text[:limit]
	}
	return text[:limit-1] + "…"
}
