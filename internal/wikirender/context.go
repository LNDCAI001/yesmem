package wikirender

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// packageIntent maps package directory → 1-line description from CLAUDE.md Key Packages.
type packageIntent struct {
	Intent string
}

// loadCLAUDEIntents reads CLAUDE.md from the project root and extracts
// descriptions from the "### Key Packages" section.
func (s *renderState) loadCLAUDEIntents() {
	dir := s.projectPath
	if dir == "" {
		return
	}
	f, err := os.Open(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		return
	}
	defer f.Close()

	s.packageIntents = map[string]string{}

	scanner := bufio.NewScanner(f)
	inKeyPackages := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "### Key Packages") {
			inKeyPackages = true
			continue
		}
		if inKeyPackages && strings.HasPrefix(line, "##") {
			break
		}
		if !inKeyPackages || !strings.HasPrefix(line, "- `") {
			continue
		}
		// Parse: - `internal/foo/` — description text
		line = strings.TrimPrefix(line, "- `")
		idx := strings.Index(line, "`")
		if idx < 0 {
			continue
		}
		pkg := line[:idx]
		pkg = strings.TrimSuffix(pkg, "/") // normalize: "internal/proxy/" → "internal/proxy"
		desc := strings.TrimPrefix(line[idx+1:], " — ")
		s.packageIntents[pkg] = desc
	}
}

// intentFor returns the CLAUDE.md Key Packages description for a package,
// or empty string if not found.
func (s *renderState) intentFor(pkg string) string {
	// Direct match
	if d, ok := s.packageIntents[pkg]; ok {
		return d
	}
	// Try matching without "internal/" prefix (e.g. "proxy" → "internal/proxy")
	if strings.HasPrefix(pkg, "internal/") {
		if d, ok := s.packageIntents[pkg[len("internal/"):]]; ok {
			return d
		}
	}
	return ""
}
