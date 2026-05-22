package hooks

import "regexp"

// destructivePatterns are hardcoded regexes for tool calls that must BLOCK
// regardless of model output. These bypass the DeepSeek roundtrip entirely
// — no network call, no downgrade — so destructive shell commands can never
// reach the OS even when the model would emit PASS.
//
// Scoped to Bash/REPL only (Edit/Write don't shell out). Each pattern is
// carefully bounded to avoid catching benign use:
//   - `rm -rf /tmp/foo` is fine; `rm -rf /` or `rm -rf ~` is not
//   - `git push origin feature/x` is fine; `git push --force origin main` is not
//   - `DROP TABLE` is always blocked (no benign use in tool calls)
var destructivePatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"rm -rf on filesystem root or home", regexp.MustCompile(`(?i)\brm\s+(?:-[a-z]*\s+)*-[a-z]*r[a-z]*f?[a-z]*\s+(?:/|~|\$HOME)(?:[\s"';)\\]|$)`)},
	{"git force-push to main/master", regexp.MustCompile(`(?i)\bgit\s+push\s+(?:--force(?:-with-lease)?|--no-verify|-f)\b[^|;&]*\b(?:origin\s+)?(?:main|master|production|prod)\b`)},
	{"git reset --hard on main/master", regexp.MustCompile(`(?i)\bgit\s+reset\s+--hard\s+\S*\b(?:main|master|production|prod)\b`)},
	{"DROP TABLE/DATABASE", regexp.MustCompile(`(?i)\bDROP\s+(?:TABLE|DATABASE|SCHEMA)\b`)},
	{"fork bomb", regexp.MustCompile(`:\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`)},
	{"dd to raw disk", regexp.MustCompile(`(?i)\bdd\s+[^|;&]*\bof=/dev/(?:(?:sd|hd|vd|xvd)[a-z]\d*|nvme\d+(?:n\d+)?(?:p\d+)?)\b`)},
	{"mkfs on raw disk", regexp.MustCompile(`(?i)\bmkfs(?:\.\w+)?\s+/dev/(?:(?:sd|hd|vd|xvd)[a-z]\d*|nvme\d+(?:n\d+)?(?:p\d+)?)\b`)},
	{"redirect to raw disk", regexp.MustCompile(`>\s*/dev/(?:(?:sd|hd|vd|xvd)[a-z]\d*|nvme\d+(?:n\d+)?(?:p\d+)?)\b`)},
	{"chmod 777 system root", regexp.MustCompile(`(?i)\bchmod\s+-R?\s+0*777\s+/(?:\s|$)`)},
	{"chown to root recursively at /", regexp.MustCompile(`(?i)\bchown\s+-R\s+\S+\s+/(?:\s|$)`)},
}

// matchDestructivePattern returns the name of the first pattern that matches,
// or "" if none match. toolDesc must already include the tool prefix
// ("Bash: " or "REPL: ") from describeToolCall.
func matchDestructivePattern(toolDesc string) string {
	for _, p := range destructivePatterns {
		if p.re.MatchString(toolDesc) {
			return p.name
		}
	}
	return ""
}
