package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

// LoadProfileFile reads a ranking profile from a file outside the configuration directory.
//
// This exists for a profile derived from a CV. A CV is personal data and cannot live in
// this repository, so the profile it produces is written somewhere private and handed to
// the engine by path. What arrives is still only preferences — skills, sectors, places —
// because that is all model.Profile can carry: there is no field for a name, an address or
// a contact detail, and the engine would have nowhere to put one.
//
// Both YAML and JSON are accepted. A private consumer generating a profile will emit JSON;
// a person writing one by hand will write YAML. YAML parses JSON, so one decoder serves
// both, but the extension is checked anyway so that a mistyped path fails on the name
// rather than on a confusing parse error twenty lines in.
func LoadProfileFile(path string, bundle *Bundle) (model.Profile, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml", ".json":
	default:
		return model.Profile{}, fmt.Errorf(
			"profile file %q must end in .yaml, .yml or .json", path)
	}

	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return model.Profile{}, fmt.Errorf("no profile file at %q", path)
	}
	if err != nil {
		return model.Profile{}, err
	}
	defer file.Close() //nolint:errcheck // nothing useful to do about it

	var profile Profile
	decoder := yaml.NewDecoder(file)
	// Strict, exactly as for a committed profile: a generated file with a misspelled key
	// would otherwise score against a preference nobody expressed.
	decoder.KnownFields(true)
	if err := decoder.Decode(&profile); err != nil {
		return model.Profile{}, fmt.Errorf("%s: %w", path, err)
	}
	if profile.Slug == "" {
		// A generated profile need not name itself; the filename is a reasonable identity
		// and is what the operator will recognise.
		profile.Slug = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	profile.sourceFile = path

	// Validate against the real configuration, so a profile naming a sector or country this
	// build does not have fails here rather than silently scoring nothing.
	staged := &Bundle{
		Dir:       bundle.Dir,
		Countries: bundle.Countries,
		Sectors:   bundle.Sectors,
		Profiles:  map[string]Profile{profile.Slug: profile},
	}
	if problems := staged.validateProfiles(); len(problems) > 0 {
		// The filename check does not apply to a file handed over by path.
		var kept []Problem
		for _, problem := range problems {
			if strings.Contains(problem.Message, "does not match the filename") {
				continue
			}
			kept = append(kept, problem)
		}
		if len(kept) > 0 {
			return model.Profile{}, &ValidationError{Problems: kept}
		}
	}
	return profile.Model(), nil
}

// WriteProfileFile writes a profile as JSON, for a consumer that generates one.
//
// Indented, because these files get read by people deciding whether the thing derived from
// their CV actually describes them.
func WriteProfileFile(path string, profile model.Profile) error {
	encoded, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o600)
}
