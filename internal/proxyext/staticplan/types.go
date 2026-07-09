// Package staticplan implements the static-content planning layer for SMM.
// It detects large, stable content regions in assembled prompts and optionally
// normalises or separates them to improve Anthropic prompt-cache reuse.
//
// Default mode is noop. All transforms are deterministic and fail-open.
package staticplan

// Mode controls what the planner is allowed to do.
type Mode string

const (
	ModeOff          Mode = "off"           // no-op; default
	ModeNormalizeText Mode = "normalize_text" // whitespace / trailing-space cleanup only
	ModeAuxTextBlock  Mode = "aux_text_block" // extract stable block to a cache-stable prefix position
	ModeExperimental  Mode = "experimental_multimodal" // reserved; not implemented in v1
)

// Config is the static planner configuration as parsed from yaml.
type Config struct {
	Enabled         bool   `yaml:"enabled"`
	Mode            Mode   `yaml:"mode"`
	MinBytes        int    `yaml:"min_bytes"`
	CacheByHash     bool   `yaml:"cache_by_hash"`
	FailOpen        bool   `yaml:"fail_open"`
	ApplyToSubagents bool  `yaml:"apply_to_subagents"`
}

// DefaultConfig returns conservative defaults: off, normalize only, fail-open.
func DefaultConfig() Config {
	return Config{
		Enabled:         false,
		Mode:            ModeOff,
		MinBytes:        12000,
		CacheByHash:     true,
		FailOpen:        true,
		ApplyToSubagents: false,
	}
}

// PlanResult is the planner's decision for a given payload.
type PlanResult struct {
	Eligible     bool
	Mode         Mode
	ContentHash  string
	Reason       string
	TransformApplied string
}
