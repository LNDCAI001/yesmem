package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/LNDCAI001/yesmem/internal/storage"
)

// runQuery executes a read-only SQL query against yesmem.db. Wraps
// storage.QueryReadOnly which validates the query and uses a connection that
// the SQLite driver opens with mode=ro and PRAGMA query_only=1.
//
// Usage:
//
//	yesmem query '<sql>' [--args '<json-array>'] [--format json|tsv]
//
// Default format is JSON: {"columns": [...], "rows": [[...], ...]}.
// Use `--format objects` for an array of {col: val, ...} objects (matches
// `sqlite3 -json` output shape), or `--format tsv` for tab-separated rows.
func runQuery(args []string) {
	sql := ""
	argsJSON := ""
	format := "json"

	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--args":
			if i+1 >= len(args) {
				queryUsage("--args needs a value")
			}
			argsJSON = args[i+1]
			i += 2
		case strings.HasPrefix(a, "--args="):
			argsJSON = strings.TrimPrefix(a, "--args=")
			i++
		case a == "--format":
			if i+1 >= len(args) {
				queryUsage("--format needs a value")
			}
			format = args[i+1]
			i += 2
		case strings.HasPrefix(a, "--format="):
			format = strings.TrimPrefix(a, "--format=")
			i++
		case a == "-h" || a == "--help":
			queryUsage("")
		default:
			if sql == "" {
				sql = a
				i++
			} else {
				queryUsage(fmt.Sprintf("unexpected argument: %q", a))
			}
		}
	}

	if sql == "" {
		queryUsage("missing SQL")
	}

	if format != "json" && format != "tsv" && format != "objects" {
		queryUsage(fmt.Sprintf("unknown format %q (want json, objects, or tsv)", format))
	}

	var queryArgs []any
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &queryArgs); err != nil {
			log.Fatalf("--args must be a JSON array: %v", err)
		}
	}

	dataDir := yesmemDataDir()
	dbPath := filepath.Join(dataDir, "yesmem.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	cols, rows, err := store.QueryReadOnly(context.Background(), sql, queryArgs)
	if err != nil {
		log.Fatalf("query: %v", err)
	}

	switch format {
	case "json":
		emitJSON(cols, rows)
	case "objects":
		emitObjects(cols, rows)
	case "tsv":
		emitTSV(cols, rows)
	}
}

func queryUsage(msg string) {
	if msg != "" {
		fmt.Fprintln(os.Stderr, "yesmem query:", msg)
	}
	fmt.Fprintln(os.Stderr, "Usage: yesmem query '<sql>' [--args '<json-array>'] [--format json|objects|tsv]")
	fmt.Fprintln(os.Stderr, "  Only SELECT and WITH statements are accepted.")
	fmt.Fprintln(os.Stderr, "  Example: yesmem query 'SELECT count(*) FROM learnings WHERE project = ?' --args '[\"yesmem\"]'")
	if msg != "" {
		os.Exit(2)
	}
	os.Exit(0)
}

func emitJSON(cols []string, rows [][]any) {
	enc := json.NewEncoder(os.Stdout)
	out := struct {
		Columns []string `json:"columns"`
		Rows    [][]any  `json:"rows"`
	}{Columns: cols, Rows: normalizeRows(rows)}
	if err := enc.Encode(out); err != nil {
		log.Fatalf("encode: %v", err)
	}
}

// emitObjects writes rows as a JSON array of {col: val, ...} objects, matching
// the output shape of `sqlite3 -json`. This is the bordmittel-friendly format
// for piping into `yesmem json` filters.
func emitObjects(cols []string, rows [][]any) {
	out := make([]map[string]any, len(rows))
	for i, r := range rows {
		obj := make(map[string]any, len(cols))
		for j, c := range cols {
			var v any
			if j < len(r) {
				v = r[j]
			}
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			obj[c] = v
		}
		out[i] = obj
	}
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(out); err != nil {
		log.Fatalf("encode: %v", err)
	}
}

// normalizeRows turns []byte values (returned by the driver for TEXT/BLOB
// columns) into strings so they survive JSON round-trips as readable text.
func normalizeRows(rows [][]any) [][]any {
	out := make([][]any, len(rows))
	for i, r := range rows {
		nr := make([]any, len(r))
		for j, v := range r {
			if b, ok := v.([]byte); ok {
				nr[j] = string(b)
			} else {
				nr[j] = v
			}
		}
		out[i] = nr
	}
	return out
}

func emitTSV(cols []string, rows [][]any) {
	w := os.Stdout
	for i, c := range cols {
		if i > 0 {
			fmt.Fprint(w, "\t")
		}
		fmt.Fprint(w, tsvEscape(c))
	}
	fmt.Fprintln(w)
	for _, r := range rows {
		for j, v := range r {
			if j > 0 {
				fmt.Fprint(w, "\t")
			}
			fmt.Fprint(w, tsvEscape(formatValue(v)))
		}
		fmt.Fprintln(w)
	}
}

func formatValue(v any) string {
	if v == nil {
		return ""
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprint(v)
}

func tsvEscape(s string) string {
	r := strings.NewReplacer("\t", "\\t", "\n", "\\n", "\r", "\\r", "\\", "\\\\")
	return r.Replace(s)
}
