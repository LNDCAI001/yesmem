package staticplan

import (
	"bytes"
	"log"
)

// Planner is the entry point for static-content analysis and transformation.
type Planner struct {
	cfg   Config
	cache *ResultCache
}

// NewPlanner creates a Planner. Returns a noop Planner when cfg.Enabled is false.
func NewPlanner(cfg Config) *Planner {
	return &Planner{
		cfg:   cfg,
		cache: NewResultCache(256),
	}
}

// Plan analyses body and returns a PlanResult.
// It never modifies body; use Apply to obtain the transformed bytes.
func (p *Planner) Plan(body []byte, isSubagent bool) PlanResult {
	if !p.cfg.Enabled || p.cfg.Mode == ModeOff {
		return PlanResult{Eligible: false, Mode: ModeOff, Reason: "disabled"}
	}
	if isSubagent && !p.cfg.ApplyToSubagents {
		return PlanResult{Eligible: false, Mode: ModeOff, Reason: "subagent_bypass"}
	}
	if len(body) < p.cfg.MinBytes {
		return PlanResult{Eligible: false, Mode: ModeOff, Reason: "below_min_bytes"}
	}

	hash := HashContent(body)

	if p.cfg.CacheByHash {
		if cached, ok := p.cache.Get(hash); ok {
			return cached
		}
	}

	result := PlanResult{
		Eligible:    true,
		Mode:        p.cfg.Mode,
		ContentHash: hash,
		Reason:      "eligible",
	}

	if p.cfg.CacheByHash {
		p.cache.Set(hash, result)
	}
	return result
}

// Apply performs the transform described by a PlanResult and returns the
// (possibly modified) body. If the plan is not eligible or the transform
// fails, the original body is returned unchanged (fail-open).
func (p *Planner) Apply(body []byte, plan PlanResult) ([]byte, string) {
	if !plan.Eligible {
		return body, ""
	}
	switch plan.Mode {
	case ModeNormalizeText:
		result := normalizeWhitespace(body)
		if result == nil {
			return body, ""
		}
		return result, "normalize_text"
	case ModeAuxTextBlock:
		// v1: aux_text_block is a stub — returns unchanged body.
		// Full extraction logic is deferred until mode proves value in production.
		log.Printf("[staticplan] aux_text_block mode is a stub in v1; returning unchanged")
		return body, ""
	case ModeExperimental:
		log.Printf("[staticplan] experimental_multimodal is not implemented in v1")
		return body, ""
	}
	return body, ""
}

// normalizeWhitespace performs safe whitespace normalization:
// - strips trailing spaces from each line
// - collapses runs of blank lines > 2 into 2
// Returns nil if the result would be byte-identical to the input.
func normalizeWhitespace(data []byte) []byte {
	lines := bytes.Split(data, []byte("\n"))
	var out [][]byte
	blankRun := 0
	changed := false
	for _, line := range lines {
		trimmed := bytes.TrimRight(line, " \t")
		if len(trimmed) != len(line) {
			changed = true
		}
		if len(trimmed) == 0 {
			blankRun++
			if blankRun > 2 {
				changed = true
				continue
			}
		} else {
			blankRun = 0
		}
		out = append(out, trimmed)
	}
	if !changed {
		return nil
	}
	return bytes.Join(out, []byte("\n"))
}
