package config

// FormalQualityMinACMMLevel is the first maturity level at which the quality
// lane may author and maintain formal models. Below L5, quality is either
// advisory or restricted to narrower testing work, so enabling the capability
// there would grant behavior the active ACMM pack does not permit.
const FormalQualityMinACMMLevel = 5

// QualityConfig controls opt-in capabilities of the quality lane. The zero
// value preserves the existing test/coverage-only behavior.
type QualityConfig struct {
	// Formal lets the quality agent identify protocol-shaped code and add a
	// Spin/Promela model, its executable verification contract, and reporting-
	// only CI. It is deliberately opt-in and is effective only at ACMM L5+.
	Formal bool `yaml:"formal,omitempty" json:"formal,omitempty"`
}

// FormalEnabled reports whether the operator opt-in and ACMM floor both allow
// formal-model work. Keeping the floor in this effective-value method means a
// level downgrade safely disables the capability without making the persisted
// config invalid or forgetting the operator's preference.
func (q QualityConfig) FormalEnabled(acmmLevel int) bool {
	return q.Formal && acmmLevel >= FormalQualityMinACMMLevel
}
