package cli

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/iobyte-labs/eu-company-discovery/pkg/config"
	eucderrors "github.com/iobyte-labs/eu-company-discovery/pkg/errors"
	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
	"github.com/iobyte-labs/eu-company-discovery/pkg/rank"
	"github.com/iobyte-labs/eu-company-discovery/pkg/store"
)

// resolveProfile finds a configured profile, or explains what is available.
func resolveProfile(bundle *config.Bundle, slug string) (config.Profile, error) {
	profile, ok := bundle.Profiles[slug]
	if ok {
		return profile, nil
	}
	available := bundle.ProfileSlugs()
	if len(available) == 0 {
		return config.Profile{}, eucderrors.New(eucderrors.KindUsage,
			"no ranking profiles are configured; add one at %s/profiles/<slug>.yaml",
			bundle.Dir)
	}
	return config.Profile{}, eucderrors.New(eucderrors.KindUsage,
		"no profile %q; configured profiles are %s", slug, strings.Join(available, ", "))
}

func newRankCmd(opts *globals) *cobra.Command {
	var (
		profileSlug string
		country     string
		sector      string
		dryRun      bool
		limit       int
		show        int
		allReasons  bool
	)
	cmd := &cobra.Command{
		Use:   "rank",
		Short: "Score companies against a profile, and record why",
		Long: "Scores every company against a configured profile and stores the result with " +
			"the reasons behind it. Scoring is deterministic and entirely offline: no network, " +
			"no model, no API key.\n\n" +
			"Scores run 0-100 and mean the same thing across profiles, because each profile is " +
			"scored over the factors it actually expresses and rescaled. A company is never " +
			"scored for belonging to an association — that is how it was found, not evidence " +
			"that it fits.\n\n" +
			"Every score is stored, including those below the profile's min_score: the " +
			"threshold is a review decision, and tuning a profile means looking at what it " +
			"rejected and why.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			bundle, err := loadConfig(opts)
			if err != nil {
				return err
			}
			configured, err := resolveProfile(bundle, profileSlug)
			if err != nil {
				return err
			}
			profile := configured.Model()

			ranker, err := rank.New(profile, rank.Options{
				Bundle: bundle, Now: time.Now().UTC(),
			})
			if err != nil {
				return eucderrors.Wrap(eucderrors.KindConfig, err, "profile %q is not usable",
					profileSlug)
			}

			db, err := openStore(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer db.Close()

			filtered := country != "" || sector != "" || limit > 0
			companies, err := db.ListCompanies(cmd.Context(), store.CompanyFilter{
				Country: country, Sector: sector, Limit: limit,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Profile %s — %q\n", profile.Slug, configured.Headline)
			fmt.Fprintf(out, "  scored over: %s\n", strings.Join(ranker.Active(), ", "))
			fmt.Fprintf(out, "  100.0 points available; min_score %.1f\n\n", profile.MinScore)

			if len(companies) == 0 {
				fmt.Fprintln(out, "No companies match, so there is nothing to rank.")
				return nil
			}

			rankings := ranker.ScoreAll(companies)
			byID := make(map[string]model.Company, len(companies))
			for _, company := range companies {
				byID[company.ID] = company
			}

			above, vetoed, gated := 0, 0, 0
			for _, ranking := range rankings {
				switch {
				case isVetoed(ranking):
					vetoed++
				case isGated(ranking):
					gated++
				}
				if ranking.Score >= profile.MinScore {
					above++
				}
			}
			fmt.Fprintf(out, "Scored %d companies: %d at or above min_score, %d vetoed, %d gated.\n\n",
				len(rankings), above, vetoed, gated)

			printRanked(out, rankings, byID, show, allReasons)

			if dryRun {
				fmt.Fprintln(out, "\nDry run — nothing was written.")
				return nil
			}

			// The foreign key on company_rankings.profile_slug means the profile must exist
			// before any score can reference it.
			if err := db.UpsertProfile(cmd.Context(), profile); err != nil {
				return eucderrors.Wrap(eucderrors.KindGeneral, err, "store profile %q", profile.Slug)
			}

			if filtered {
				written, err := db.PutRankings(cmd.Context(), rankings)
				if err != nil {
					return eucderrors.Wrap(eucderrors.KindGeneral, err, "store rankings")
				}
				fmt.Fprintf(out, "\nWrote %d ranking(s) for profile %s, leaving the rest of the "+
					"table alone because this run was filtered.\n", written, profile.Slug)
			} else {
				result, err := db.ReplaceRankings(cmd.Context(), profile.Slug, rankings)
				if err != nil {
					return eucderrors.Wrap(eucderrors.KindGeneral, err, "store rankings")
				}
				fmt.Fprintf(out, "\nWrote %d ranking(s) for profile %s, replacing %d previous one(s).\n",
					result.Written, profile.Slug, result.Removed)
			}
			fmt.Fprintf(out, "`eucd companies top --profile %s` shows them; "+
				"add --explain for these reasons.\n", profile.Slug)
			return nil
		},
	}
	cmd.Flags().StringVar(&profileSlug, "profile", "", "profile slug from config/profiles (required)")
	cmd.Flags().StringVar(&country, "country", "", "only score companies in this country")
	cmd.Flags().StringVar(&sector, "sector", "", "only score companies in this sector")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "score and report, write nothing")
	// Unlike enrich, this defaults to everything: ranking is local and cheap, and a partly
	// ranked table would let `companies top` present a subset as if it were the ranking.
	cmd.Flags().IntVar(&limit, "limit", 0, "score at most this many companies (0 for all)")
	cmd.Flags().IntVar(&show, "show", 10, "print this many top companies with their reasons (0 for all)")
	cmd.Flags().BoolVar(&allReasons, "all-reasons", false, "include reasons that scored nothing")
	_ = cmd.MarkFlagRequired("profile")
	return cmd
}

func isVetoed(r model.Ranking) bool {
	return len(r.Reasons) == 1 && r.Reasons[0].Code == rank.ReasonExcludeVeto
}

func isGated(r model.Ranking) bool {
	return len(r.Reasons) == 1 && r.Reasons[0].Code == rank.ReasonMustHaveMissing
}

func printRanked(
	out interface{ Write([]byte) (int, error) },
	rankings []model.Ranking, byID map[string]model.Company, show int, allReasons bool,
) {
	shown := 0
	for _, ranking := range rankings {
		if show > 0 && shown >= show {
			break
		}
		if isVetoed(ranking) || isGated(ranking) {
			continue
		}
		company := byID[ranking.CompanyID]
		shown++
		fmt.Fprintf(out, "%3d. %6.1f  %-44s %s/%s  %s\n", shown, ranking.Score,
			truncateForDisplay(company.Name, 44), company.Country,
			orDash(company.Region), company.CanonicalDomain)

		reasons := ranking.Reasons
		if !allReasons {
			reasons = rank.Significant(reasons)
		}
		for _, reason := range reasons {
			fmt.Fprintf(out, "           %+6.1f  %-22s %s\n",
				reason.Points, reason.Code, reason.Detail)
		}
		fmt.Fprintln(out)
	}

	// Vetoed and gated companies are reported, not hidden: "where did the company I
	// expected go?" is the first question anyone asks of a ranking.
	printExcluded(out, rankings, byID, "Vetoed", isVetoed)
	printExcluded(out, rankings, byID, "Gated", isGated)
}

func printExcluded(
	out interface{ Write([]byte) (int, error) },
	rankings []model.Ranking, byID map[string]model.Company,
	label string, match func(model.Ranking) bool,
) {
	var matching []model.Ranking
	for _, ranking := range rankings {
		if match(ranking) {
			matching = append(matching, ranking)
		}
	}
	if len(matching) == 0 {
		return
	}
	sort.Slice(matching, func(i, j int) bool {
		return byID[matching[i].CompanyID].Name < byID[matching[j].CompanyID].Name
	})
	limit := min(3, len(matching))
	fmt.Fprintf(out, "%s (%d of %d shown):\n", label, limit, len(matching))
	for _, ranking := range matching[:limit] {
		fmt.Fprintf(out, "   %-46s %s: %s\n",
			truncateForDisplay(byID[ranking.CompanyID].Name, 46),
			ranking.Reasons[0].Code, ranking.Reasons[0].Detail)
	}
	fmt.Fprintln(out)
}

func orDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

// newCompaniesTopCmd lists companies by score. It is a separate command rather than a flag
// on `companies list` because it changes the ordering, the row set and the columns at once
// — and `list --json` emits the companies envelope, which has no place for a score.
func newCompaniesTopCmd(opts *globals) *cobra.Command {
	var (
		profileSlug string
		minScore    float64
		includeZero bool
		country     string
		sector      string
		withContact bool
		limit       int
		explain     bool
	)
	cmd := &cobra.Command{
		Use:   "top",
		Short: "List companies by score against a profile",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			bundle, err := loadConfig(opts)
			if err != nil {
				return err
			}
			configured, err := resolveProfile(bundle, profileSlug)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("min-score") {
				minScore = configured.MinScore
			}

			db, err := openStore(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer db.Close()

			rows, err := db.TopCompanies(cmd.Context(), store.RankingFilter{
				ProfileSlug: profileSlug, MinScore: minScore, ExcludeZero: !includeZero,
				Country: country, Sector: sector, WithContact: withContact, Limit: limit,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				stored, _ := db.CountRankings(cmd.Context(), profileSlug)
				if stored == 0 {
					fmt.Fprintf(out, "No ranking is stored for profile %s. "+
						"Run `eucd rank --profile %s`.\n", profileSlug, profileSlug)
					return nil
				}
				fmt.Fprintf(out, "No company scores at or above %.1f for profile %s "+
					"(%d ranked in total).\n", minScore, profileSlug, stored)
				return nil
			}

			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "RANK\tSCORE\tNAME\tCOUNTRY\tCONTACT\tKIND\tTOP REASON")
			for index, row := range rows {
				contact, kind := "—", "—"
				if best, ok := row.Company.BestContact(); ok {
					contact, kind = best.Address, string(best.Kind)
				}
				top := "—"
				if reason, ok := rank.TopReason(rank.Significant(row.Ranking.Reasons)); ok {
					top = reason.Code + " " + reason.Detail
				}
				fmt.Fprintf(tw, "%d\t%.1f\t%s\t%s\t%s\t%s\t%s\n",
					index+1, row.Ranking.Score, truncateForDisplay(row.Company.Name, 38),
					row.Company.Country, contact, kind, truncateForDisplay(top, 46))
			}
			if err := tw.Flush(); err != nil {
				return err
			}

			if explain {
				fmt.Fprintln(out)
				for index, row := range rows {
					fmt.Fprintf(out, "%d. %.1f  %s\n", index+1, row.Ranking.Score, row.Company.Name)
					for _, reason := range rank.Significant(row.Ranking.Reasons) {
						fmt.Fprintf(out, "      %+6.1f  %-22s %s\n",
							reason.Points, reason.Code, reason.Detail)
					}
					fmt.Fprintln(out)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&profileSlug, "profile", "", "profile slug (required)")
	cmd.Flags().Float64Var(&minScore, "min-score", 0, "minimum score (default: the profile's)")
	cmd.Flags().BoolVar(&includeZero, "include-zero", false, "include vetoed and gated companies")
	cmd.Flags().StringVar(&country, "country", "", "filter by country code")
	cmd.Flags().StringVar(&sector, "sector", "", "filter by sector slug")
	cmd.Flags().BoolVar(&withContact, "with-contact", false, "only companies with a contact")
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum rows (0 for no limit)")
	cmd.Flags().BoolVar(&explain, "explain", false, "print every company's reasons")
	_ = cmd.MarkFlagRequired("profile")
	return cmd
}
