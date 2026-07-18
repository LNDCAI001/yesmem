package daemon

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/LNDCAI001/yesmem/internal/capfile"
)

// parseScriptsParam decodes the `scripts` param of save_cap into validated
// ScriptMeta values, applying defaults and provider-form conversion for REPL
// bodies so storage holds the canonical adapter-decoupled form.
func parseScriptsParam(s string) ([]ScriptMeta, error) {
	if s == "" {
		return nil, fmt.Errorf("scripts is required (JSON array)")
	}
	var raw []ScriptMeta
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("scripts: invalid JSON: %w", err)
	}
	return normalizeScripts(raw)
}

func scriptsFromSaveCapParams(name string, params map[string]any) ([]ScriptMeta, error) {
	if scriptsJSON := stringOr(params, "scripts", ""); scriptsJSON != "" {
		return parseScriptsParam(scriptsJSON)
	}

	handlerBash := stringOr(params, "handler_bash", "")
	handlerREPL := stringOr(params, "handler_repl", "")
	if handlerBash == "" && handlerREPL == "" {
		return nil, fmt.Errorf("'scripts' or at least one legacy handler ('handler_bash' or 'handler_repl') is required")
	}

	schema := stringOr(params, "schema", "")
	var scripts []ScriptMeta
	if handlerREPL != "" {
		scripts = append(scripts, ScriptMeta{
			Name:    name,
			Kind:    "tool",
			Runtime: "repl",
			Lang:    "javascript",
			Body:    handlerREPL,
			Schema:  schema,
		})
	}
	if handlerBash != "" {
		scriptName := name
		kind := "tool"
		scriptSchema := schema
		if handlerREPL != "" {
			scriptName = name + "_bash"
			kind = "handler"
			scriptSchema = ""
		} else if scriptSchema == "" {
			scriptSchema = "{}"
		}
		scripts = append(scripts, ScriptMeta{
			Name:    scriptName,
			Kind:    kind,
			Runtime: "bash",
			Lang:    "bash",
			Body:    handlerBash,
			Schema:  scriptSchema,
		})
	}
	return normalizeScripts(scripts)
}

func normalizeScripts(raw []ScriptMeta) ([]ScriptMeta, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("scripts: at least one script required")
	}
	seen := map[string]bool{}
	for i := range raw {
		sc := &raw[i]
		if sc.Name == "" {
			return nil, fmt.Errorf("scripts[%d]: name is required", i)
		}
		if seen[sc.Name] {
			return nil, fmt.Errorf("scripts: duplicate name %q", sc.Name)
		}
		seen[sc.Name] = true

		if sc.Kind == "" {
			sc.Kind = "tool"
		}
		if sc.Kind != "tool" && sc.Kind != "handler" {
			return nil, fmt.Errorf("script %q: kind must be tool or handler, got %q", sc.Name, sc.Kind)
		}

		if sc.Runtime == "" {
			sc.Runtime = "repl"
		}
		if sc.Runtime != "repl" && sc.Runtime != "bash" {
			return nil, fmt.Errorf("script %q: runtime must be repl or bash, got %q", sc.Name, sc.Runtime)
		}

		if sc.Body == "" {
			return nil, fmt.Errorf("script %q: body is required", sc.Name)
		}

		if sc.Kind == "tool" && sc.Schema == "" {
			if sc.Runtime == "repl" {
				sc.Schema = capfile.DeriveSchema(sc.Body)
			} else {
				return nil, fmt.Errorf("script %q: tool with runtime=bash requires explicit schema", sc.Name)
			}
		}
		if sc.Kind == "handler" && sc.Schema != "" {
			return nil, fmt.Errorf("script %q: handlers cannot have a schema", sc.Name)
		}

		if sc.Runtime == "repl" {
			sc.Body = capfile.GenericToProvider(sc.Body, capfile.DefaultAdapters())
		}
	}
	return raw, nil
}

// detectRequiresFromScriptMetas scans every script body for adapter primitive
// calls and returns the set of detected primitive names.
func detectRequiresFromScriptMetas(scripts []ScriptMeta) []string {
	primitives := []string{"file", "store", "web"}
	seen := map[string]bool{}
	var out []string
	for _, sc := range scripts {
		for _, p := range primitives {
			if !seen[p] && strings.Contains(sc.Body, p+"(") {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// scriptToJSFunc renders a ScriptMeta as a JS function string suitable for
// passing to registerTool. REPL scripts are returned as-is (they are already
// async function expressions). Bash scripts are wrapped so the generated
// JS function shells out via the sh() adapter primitive.
func scriptToJSFunc(sc ScriptMeta) string {
	if sc.Runtime == "bash" {
		return fmt.Sprintf("async () => sh(%q)", sc.Body)
	}
	return sc.Body
}

// mergeScripts merges incoming scripts into existing scripts by name.
// Scripts in existing that have a matching name in incoming are replaced;
// unmatched existing scripts are kept; new names in incoming are appended.
// When incoming is empty the caller's upstream validation (normalizeScripts)
// rejects it before we are reached; the nil-return is defensive.
func mergeScripts(existing, incoming []ScriptMeta) []ScriptMeta {
	if len(incoming) == 0 {
		return nil
	}
	byName := make(map[string]int)
	for i, sc := range existing {
		byName[sc.Name] = i
	}
	merged := make([]ScriptMeta, len(existing))
	copy(merged, existing)
	for _, sc := range incoming {
		if idx, ok := byName[sc.Name]; ok {
			merged[idx] = sc
		} else {
			merged = append(merged, sc)
		}
	}
	return merged
}
