package cli

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	eucderrors "github.com/iobyte-labs/eu-company-discovery/pkg/errors"
	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
	"github.com/iobyte-labs/eu-company-discovery/pkg/normalize"
	"github.com/iobyte-labs/eu-company-discovery/pkg/store"
)

func newImportCmd(opts *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import companies from outside this engine",
	}
	cmd.AddCommand(newImportLegacyCmd(opts))
	cmd.AddCommand(newImportDatasetCmd(opts))
	return cmd
}

// legacyColumns are the columns of the base version's companies.csv. Named here so a file
// with a changed header is rejected with a clear message rather than silently importing
// blanks.
var legacyRequiredColumns = []string{"company_name", "status"}

func newImportLegacyCmd(opts *globals) *cobra.Command {
	var (
		country         string
		associationSlug string
		sectors         []string
		dryRun          bool
	)
	cmd := &cobra.Command{
		Use:   "legacy-csv <companies.csv>",
		Short: "Import the base version's companies.csv",
		Long: `Imports discovery facts from the predecessor project's companies.csv — the
company, its website, its careers page and the contact addresses that were found published.

It deliberately does NOT import application state. The status column (approved, sent,
replied, opted-out), the cover letters and the send timestamps are personal application
data, which does not belong in this engine or in a public repository. They belong to the
private consumer.

That split leaves one thing you must do separately, and it matters: the predecessor's
sent log and opt-out list have to be imported into the private consumer's suppression list
before it is ever allowed to send. Nothing in this engine's data would reveal that those
companies were already contacted, so without that step the scaled system will email them a
second time. This command prints a reminder with the counts it saw.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if country == "" {
				return eucderrors.New(eucderrors.KindUsage,
					"--country is required: a company name only identifies a company within "+
						"one country, so an import without it cannot deduplicate")
			}
			country = strings.ToLower(country)
			if associationSlug == "" {
				associationSlug = "legacy_import"
			}

			file, err := os.Open(args[0])
			if err != nil {
				return eucderrors.Wrap(eucderrors.KindUsage, err, "open %q", args[0])
			}
			// Read-only: the read's own error is checked below, and releasing a file
			// descriptor cannot fail in a way a caller could act on.
			defer func() { _ = file.Close() }()

			rows, err := readLegacyCSV(file)
			if err != nil {
				return eucderrors.Wrap(eucderrors.KindUsage, err, "read %q", args[0])
			}

			out := cmd.OutOrStdout()
			summary := legacySummary{}

			var db *store.Store
			if !dryRun {
				if db, err = openStore(cmd.Context(), opts); err != nil {
					return err
				}
				defer db.Close()
			}

			retrievedAt := time.Now().UTC()
			for index, row := range rows {
				company, skip := legacyRowToCompany(row, country, associationSlug, sectors, retrievedAt)
				summary.note(row)
				if skip != "" {
					summary.skipped++
					summary.skipReasons[skip]++
					continue
				}
				if dryRun {
					summary.wouldImport++
					if summary.wouldImport <= 10 {
						fmt.Fprintf(out, "  WOULD IMPORT  %-45s %s\n",
							truncateForDisplay(company.Name, 45), company.Website)
					}
					continue
				}
				result, err := db.UpsertCompany(cmd.Context(), company)
				if err != nil {
					return eucderrors.Wrap(eucderrors.KindGeneral, err,
						"row %d (%q)", index+2, row["company_name"])
				}
				if result.Created {
					summary.created++
				} else {
					summary.updated++
				}
				if result.Conflict != "" {
					fmt.Fprintf(out, "  identity collision: %s\n", result.Conflict)
				}
			}

			summary.print(out, len(rows), dryRun)
			return nil
		},
	}
	cmd.Flags().StringVar(&country, "country", "",
		"ISO country code for every row in the file (required)")
	cmd.Flags().StringVar(&associationSlug, "association", "legacy_import",
		"provenance label recorded against each imported company")
	cmd.Flags().StringSliceVar(&sectors, "sector", nil,
		"sector slug to tag imported companies with; repeatable")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be imported, write nothing")
	return cmd
}

func readLegacyCSV(r io.Reader) ([]map[string]string, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1 // the base version's letters contain embedded newlines
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	index := map[string]int{}
	for position, name := range header {
		index[strings.TrimSpace(name)] = position
	}
	for _, required := range legacyRequiredColumns {
		if _, ok := index[required]; !ok {
			return nil, fmt.Errorf("the file has no %q column, so it is not a companies.csv "+
				"from the base version (columns found: %s)", required, strings.Join(header, ", "))
		}
	}

	var rows []map[string]string
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		row := make(map[string]string, len(index))
		for name, position := range index {
			if position < len(record) {
				row[name] = strings.TrimSpace(record[position])
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// legacyRowToCompany maps one base-version row onto a company. The second return value is a
// non-empty skip reason when the row should not be imported.
func legacyRowToCompany(
	row map[string]string, country, associationSlug string, sectors []string, retrievedAt time.Time,
) (model.Company, string) {
	name := normalize.DisplayName(row["company_name"])
	if name == "" {
		return model.Company{}, "no company name"
	}

	company := model.Company{
		Name:        name,
		Country:     country,
		Website:     row["website"],
		Description: row["short_description"],
		CareersURL:  row["careers_url"],
		Sectors:     sectors,
		Quality:     model.QualityLow,
		Sources: []model.Source{{
			AssociationSlug: associationSlug,
			SourceURL:       "legacy:companies.csv",
			RawName:         row["company_name"],
			RetrievedAt:     retrievedAt,
		}},
	}
	if language := row["language"]; language != "" {
		company.Languages = []string{strings.ToLower(language)}
	}
	if category := row["category"]; category != "" {
		company.Keywords = []string{category}
	}

	// Contacts. The base version recorded where an address was found in email_source, which
	// is exactly the provenance this engine requires, so a row that has it imports as
	// published. A row without it cannot be called published, and is imported as unverified
	// rather than being given a provenance it does not have.
	seen := map[string]bool{}
	addAddress := func(raw string) {
		address := normalize.Email(raw)
		if address == "" || seen[address] {
			return
		}
		// An individual's address is never imported, whatever the source file holds.
		if normalize.LooksPersonal(address) {
			return
		}
		seen[address] = true

		contact := model.Contact{
			Address: address,
			Kind:    model.ContactKind(normalize.ClassifyLocalPart(address)),
			FoundAt: retrievedAt,
			Notes:   "imported from the base version",
		}
		if source := normalize.URL(row["email_source"]); source != "" {
			contact.Verification = model.VerificationPublished
			contact.FoundOnURL = source
		} else {
			contact.Verification = model.VerificationUnverified
		}
		company.Contacts = append(company.Contacts, contact)
	}

	addAddress(row["email"])
	for _, extra := range strings.FieldsFunc(row["all_emails"], func(r rune) bool {
		return r == ';' || r == ',' || r == ' ' || r == '\n'
	}) {
		addAddress(extra)
	}

	if company.Website != "" && len(company.Contacts) > 0 {
		company.Quality = model.QualityMedium
	}
	return company, ""
}

type legacySummary struct {
	created     int
	updated     int
	skipped     int
	wouldImport int
	skipReasons map[string]int
	statuses    map[string]int
	// alreadyContacted counts rows the base version had already acted on. These are the
	// rows whose state must reach the private consumer's suppression list.
	alreadyContacted int
	optedOut         int
}

func (s *legacySummary) note(row map[string]string) {
	if s.statuses == nil {
		s.statuses = map[string]int{}
		s.skipReasons = map[string]int{}
	}
	status := row["status"]
	if status == "" {
		status = "pending"
	}
	s.statuses[status]++
	switch status {
	case "sent", "replied":
		s.alreadyContacted++
	case "opted-out":
		s.optedOut++
	}
}

func (s *legacySummary) print(out io.Writer, totalRows int, dryRun bool) {
	fmt.Fprintf(out, "\n%d row(s) read.\n", totalRows)
	if dryRun {
		fmt.Fprintf(out, "%d would be imported, %d skipped — dry run, nothing written.\n",
			s.wouldImport, s.skipped)
	} else {
		fmt.Fprintf(out, "%d new, %d updated, %d skipped.\n", s.created, s.updated, s.skipped)
	}
	for _, reason := range sortedMapKeys(s.skipReasons) {
		fmt.Fprintf(out, "  skipped (%s): %d\n", reason, s.skipReasons[reason])
	}

	fmt.Fprintln(out, "\nStatus column in the source file (NOT imported — it is application state):")
	for _, status := range sortedMapKeys(s.statuses) {
		fmt.Fprintf(out, "  %-18s %d\n", status, s.statuses[status])
	}

	// The whole point of the reminder: this is the one way the scaled system can
	// accidentally re-contact someone.
	if s.alreadyContacted > 0 || s.optedOut > 0 {
		fmt.Fprintf(out, "\n⚠  %d company/companies were already contacted by the base version",
			s.alreadyContacted)
		if s.optedOut > 0 {
			fmt.Fprintf(out, " and %d opted out", s.optedOut)
		}
		fmt.Fprint(out, ".\n")
		fmt.Fprintln(out, "   Import the base version's logs/sent_log.csv and config/optout.txt "+
			"into the private")
		fmt.Fprintln(out, "   consumer's suppression list before it is allowed to send anything, "+
			"or it will")
		fmt.Fprintln(out, "   email them again. This engine holds no record that they were "+
			"contacted.")
	}
}

func sortedMapKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
