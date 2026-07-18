package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/LNDCAI001/yesmem/internal/codescan"
	"github.com/LNDCAI001/yesmem/internal/storage"
	"github.com/LNDCAI001/yesmem/internal/wikirender"
)

func runWikiRender(args []string) error {
	fs := flag.NewFlagSet("wiki-render", flag.ExitOnError)
	project := fs.String("project", "", "Project name (required)")
	out := fs.String("out", "", "Output dir (default ~/.claude/yesmem/wiki/<project>)")
	quiet := fs.Bool("quiet", false, "Suppress progress output")
	jsonLine := fs.Bool("json", true, "Emit final JSON status line on stdout")
	scan := fs.Bool("scan", false, "Use CBM scanner for live code graph (imports, calls)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" {
		fs.Usage()
		return fmt.Errorf("--project is required")
	}
	if *out == "" {
		home, _ := os.UserHomeDir()
		*out = filepath.Join(home, ".claude", "yesmem", "wiki", *project)
	}

	store, err := storage.Open(filepath.Join(yesmemDataDir(), "yesmem.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	var graph *codescan.CodeGraph
	if *scan {
		dir := store.ResolveProjectPath(*project)
		if dir != "" && codescan.FindCBMBinary() != "" {
			scanner := codescan.NewCBMScanner()
			if sr, err := scanner.Scan(dir); err == nil {
				graph = codescan.BuildCodeGraph(sr)
				if raw, err := json.Marshal(sr); err == nil {
					store.SaveProjectScan(&storage.ProjectScanRow{
						Project: *project, ScanJSON: string(raw),
					})
				}
			}
		}
	}

	r, err := wikirender.Render(context.Background(), wikirender.RenderConfig{
		Project:   *project,
		OutputDir: *out,
		Store:     store,
		CodeGraph: graph,
		Quiet:     *quiet,
	})
	if err != nil {
		return err
	}

	if *jsonLine {
		j, _ := json.Marshal(map[string]any{
			"project": r.Project, "learnings": r.Learnings,
			"quarantined": r.Quarantined, "topics": r.Topics,
			"files": r.Files, "sessions": r.Sessions,
			"contradictions": r.Contradictions,
			"out": *out, "built_at": r.BuiltAt,
		})
		fmt.Println(string(j))
	} else if !*quiet {
		fmt.Printf("rendered project=%s learnings=%d topics=%d files=%d sessions=%d in %dms\n",
			r.Project, r.Learnings, r.Topics, r.Files, r.Sessions, r.DurationMs)
	}
	return nil
}
