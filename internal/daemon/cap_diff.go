package daemon

import (
	"strings"

	"github.com/LNDCAI001/yesmem/internal/textutil"
)

// DiffClass tags a proposed cap-body change as either a small fix that the
// auto-correct loop may persist on its own, or a substantial change that
// must go through the cap_proposed staging path for explicit user approval.
type DiffClass int

const (
	DiffMinimal DiffClass = iota
	DiffSubstantial
)

// Tunables. Empirically: minor flag/typo fixes stay below five new unique
// lines and above 0.85 token similarity; full handler rewrites cross both.
const (
	maxAddedLines = 5
	minSimilarity = 0.85
)

// ClassifyCapDiff decides how the auto-correct pipeline must handle a
// proposed change. A change counts as DiffSubstantial when ANY of the
// following holds: the proposed body adds more than maxAddedLines new
// unique lines, the proposed body invokes a command that did not appear
// in the original (no-new-binary gate, runs before similarity so a
// 1-line swap that injects a fresh network call cannot ride a 1.0
// similarity score through), or the token similarity to the original
// drops below minSimilarity. Otherwise it is DiffMinimal. Identical
// inputs are always DiffMinimal.
func ClassifyCapDiff(original, proposed string) DiffClass {
	if original == proposed {
		return DiffMinimal
	}

	origLines := strings.Split(strings.TrimRight(original, "\n"), "\n")
	propLines := strings.Split(strings.TrimRight(proposed, "\n"), "\n")

	if lineDelta(origLines, propLines) > maxAddedLines {
		return DiffSubstantial
	}

	origBins := extractCommandTokens(original)
	propBins := extractCommandTokens(proposed)
	for tok := range propBins {
		if _, ok := origBins[tok]; !ok {
			return DiffSubstantial
		}
	}

	origToks := textutil.Tokenize(original)
	propToks := textutil.Tokenize(proposed)
	if textutil.TokenSimilarity(origToks, propToks) < minSimilarity {
		return DiffSubstantial
	}

	return DiffMinimal
}

// lineDelta counts proposed lines that did not appear verbatim in the
// original. Pure-whitespace lines and exact duplicates are ignored so that
// blank-line reformatting does not push a fix into DiffSubstantial.
func lineDelta(orig, prop []string) int {
	origSet := make(map[string]struct{}, len(orig))
	for _, line := range orig {
		origSet[strings.TrimSpace(line)] = struct{}{}
	}
	added := 0
	for _, line := range prop {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if _, ok := origSet[trimmed]; !ok {
			added++
		}
	}
	return added
}

// extractCommandTokens returns the set of distinct command tokens invoked
// at the start of each statement in a shell-like body. Statements split on
// newline and on the simple separators ";", "&&", "||", "|". Leading
// variable assignments (FOO=bar cmd) are stripped, comments and empty
// lines ignored, and obviously syntactic tokens (parentheses, redirects,
// $-expansions) filtered out. The textual split does not respect quoting,
// which is good enough for the no-new-binary gate in ClassifyCapDiff.
func extractCommandTokens(body string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, raw := range splitShellStatements(body) {
		s := strings.TrimSpace(raw)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		for {
			sp := strings.IndexAny(s, " \t")
			if sp <= 0 {
				break
			}
			head := s[:sp]
			if i := strings.Index(head, "="); i > 0 {
				s = strings.TrimSpace(s[sp+1:])
				if s == "" {
					break
				}
				continue
			}
			break
		}
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		sp := strings.IndexAny(s, " \t")
		var tok string
		if sp == -1 {
			tok = s
		} else {
			tok = s[:sp]
		}
		tok = strings.Trim(tok, "\"'`")
		if tok == "" {
			continue
		}
		if strings.ContainsAny(tok, "(){}<>") || strings.HasPrefix(tok, "$") {
			continue
		}
		out[tok] = struct{}{}
	}
	return out
}

// splitShellStatements splits a body into shell statements on \n, ;, &&,
// ||, and |. The split is purely textual and does not respect quoting.
func splitShellStatements(body string) []string {
	work := body
	for _, sep := range []string{"&&", "||"} {
		work = strings.ReplaceAll(work, sep, "\n")
	}
	work = strings.ReplaceAll(work, ";", "\n")
	work = strings.ReplaceAll(work, "|", "\n")
	return strings.Split(work, "\n")
}
