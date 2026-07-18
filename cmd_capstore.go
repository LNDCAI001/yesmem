package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/LNDCAI001/yesmem/internal/daemon"
)

func runCapStoreCLI(args []string) {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: yesmem cap-store <capability> <action> <table> [where] [args_json]")
		fmt.Fprintln(os.Stderr, "  Actions: query, upsert, delete")
		fmt.Fprintln(os.Stderr, "  Example: yesmem cap-store telegram query config \"key = ?\" '[\"bot_token\"]'")
		os.Exit(1)
	}

	capability := args[0]
	action := args[1]
	table := args[2]

	params := map[string]any{
		"capability": capability,
		"action":     action,
		"table":      table,
	}

	switch action {
	case "query":
		if len(args) > 3 {
			params["where"] = args[3]
		}
		if len(args) > 4 {
			params["args"] = args[4]
		}
	case "upsert":
		if len(args) > 3 {
			params["data"] = args[3]
		}
	case "delete":
		if len(args) > 3 {
			params["where"] = args[3]
		}
		if len(args) > 4 {
			params["args"] = args[4]
		}
	}

	home, _ := os.UserHomeDir()
	dataDir := filepath.Join(home, ".claude", "yesmem")
	client, err := daemon.Dial(dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
		os.Exit(1)
	}

	result, err := client.Call("cap_store", params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cap_store: %v\n", err)
		os.Exit(1)
	}

	var parsed any
	if json.Unmarshal(result, &parsed) == nil {
		out, _ := json.MarshalIndent(parsed, "", "  ")
		fmt.Println(string(out))
	} else {
		fmt.Println(string(result))
	}
}
