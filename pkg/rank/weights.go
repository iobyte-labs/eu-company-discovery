package rank

import (
	"errors"
	"fmt"
)

// Weights is how much each sub-factor is worth, out of 100.
//
// Weights are deliberately NOT configurable from a profile file. Two profiles whose weights
// differ produce scores that cannot be compared, and MinScore is a single number with no
// per-profile calibration — so a tunable weight table would quietly make thresholds mean
// different things in different files. The knobs an operator actually needs (skill weights,
// exclude terms, min_score) are all in the profile already.
//
// The type is exported so a private consumer can experiment, and so the table is readable.
type Weights struct {
	Skills float64
	// Sector is the vocabulary match. SectorDeclaredMax is the much smaller amount a
	// company earns merely for being filed under the sector by a directory — see the note
	// on association membership in Score.
	Sector            float64
	SectorDeclaredMax float64
	GeoCountry        float64
	GeoRegion         float64
	Contact           float64
	EvidenceQuality   float64
	EvidenceFreshness float64
	Language          float64
	Size              float64
}

// DefaultWeights is the committed table. It sums to 100.
func DefaultWeights() Weights {
	return Weights{
		Skills: 40,
		// Of the 20 sector points, at most 4 come from a directory's own filing and the
		// rest must be earned from the company's own words.
		Sector:            20,
		SectorDeclaredMax: 4,
		GeoCountry:        10,
		GeoRegion:         5,
		Contact:           10,
		EvidenceQuality:   5,
		EvidenceFreshness: 3,
		Language:          5,
		Size:              2,
	}
}

// Total is the sum of every sub-factor weight.
func (w Weights) Total() float64 {
	return w.Skills + w.Sector + w.GeoCountry + w.GeoRegion + w.Contact +
		w.EvidenceQuality + w.EvidenceFreshness + w.Language + w.Size
}

// Validate rejects a table that would make scores meaningless.
func (w Weights) Validate() error {
	named := []struct {
		name  string
		value float64
	}{
		{"Skills", w.Skills}, {"Sector", w.Sector},
		{"SectorDeclaredMax", w.SectorDeclaredMax},
		{"GeoCountry", w.GeoCountry}, {"GeoRegion", w.GeoRegion},
		{"Contact", w.Contact}, {"EvidenceQuality", w.EvidenceQuality},
		{"EvidenceFreshness", w.EvidenceFreshness}, {"Language", w.Language},
		{"Size", w.Size},
	}
	for _, item := range named {
		if item.value < 0 {
			return fmt.Errorf("weight %s is negative (%.1f)", item.name, item.value)
		}
	}
	if w.SectorDeclaredMax > w.Sector {
		return errors.New("SectorDeclaredMax exceeds Sector, so a directory's filing could " +
			"outweigh what a company says about itself")
	}
	if total := w.Total(); total < 99.999 || total > 100.001 {
		return fmt.Errorf("weights sum to %.3f, not 100", total)
	}
	return nil
}
