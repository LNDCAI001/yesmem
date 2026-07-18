package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/LNDCAI001/yesmem/internal/capblob"
	"github.com/LNDCAI001/yesmem/internal/daemon"
	"github.com/LNDCAI001/yesmem/internal/storage"
)

// rpcStore satisfies capblob.Store by routing every call through the daemon's
// cap_store RPC. Used by cap-blob-put / cap-blob-get CLI subcommands.
type rpcStore struct {
	client *daemon.SocketClient
}

func dialCapStore() (*rpcStore, func(), error) {
	client, err := daemon.DialTimeout(yesmemDataDir(), 5*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to daemon: %w", err)
	}
	client.SetDeadline(time.Now().Add(30 * time.Second))
	return &rpcStore{client: client}, func() { client.Close() }, nil
}

func (r *rpcStore) call(params map[string]any) (json.RawMessage, error) {
	return r.client.Call("cap_store", params)
}

func (r *rpcStore) CapStoreCreateTable(capName, tableName string, columns []storage.ColumnDef) error {
	colsRaw := make([]map[string]string, len(columns))
	for i, c := range columns {
		colsRaw[i] = map[string]string{"name": c.Name, "type": c.Type}
	}
	colsJSON, err := json.Marshal(colsRaw)
	if err != nil {
		return err
	}
	_, err = r.call(map[string]any{
		"action":     "create_table",
		"capability": capName,
		"table":      tableName,
		"columns":    string(colsJSON),
	})
	return err
}

func (r *rpcStore) CapStoreUpsert(capName, tableName string, data map[string]any) (int64, error) {
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	raw, err := r.call(map[string]any{
		"action":     "upsert",
		"capability": capName,
		"table":      tableName,
		"data":       string(dataJSON),
	})
	if err != nil {
		return 0, err
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(raw, &resp)
	return resp.ID, nil
}

func (r *rpcStore) CapStoreQuery(capName, tableName, where string, args []any, limit int) ([]map[string]any, error) {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	raw, err := r.call(map[string]any{
		"action":     "query",
		"capability": capName,
		"table":      tableName,
		"where":      where,
		"args":       string(argsJSON),
		"limit":      limit,
	})
	if err != nil {
		return nil, err
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		var wrapped struct {
			Rows []map[string]any `json:"rows"`
		}
		if werr := json.Unmarshal(raw, &wrapped); werr == nil {
			return wrapped.Rows, nil
		}
		return nil, fmt.Errorf("decode query result: %w (raw=%s)", err, string(raw))
	}
	return rows, nil
}

func (r *rpcStore) CapStoreDelete(capName, tableName, where string, args []any) (int64, error) {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return 0, err
	}
	raw, err := r.call(map[string]any{
		"action":     "delete",
		"capability": capName,
		"table":      tableName,
		"where":      where,
		"args":       string(argsJSON),
	})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Deleted int64 `json:"deleted"`
	}
	_ = json.Unmarshal(raw, &resp)
	return resp.Deleted, nil
}

func runCapBlobPut() {
	cap, key := "", ""
	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cap":
			if i+1 < len(args) {
				cap = args[i+1]
				i++
			}
		case "--key":
			if i+1 < len(args) {
				key = args[i+1]
				i++
			}
		case "-h", "--help":
			printCapBlobPutHelp()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			os.Exit(2)
		}
	}
	if cap == "" || key == "" {
		fmt.Fprintln(os.Stderr, "cap-blob-put: --cap and --key are required")
		printCapBlobPutHelp()
		os.Exit(2)
	}
	store, done, err := dialCapStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cap-blob-put: %v\n", err)
		os.Exit(1)
	}
	defer done()
	if err := capblob.Put(store, cap, key, os.Stdin, capblob.DefaultChunkSize); err != nil {
		fmt.Fprintf(os.Stderr, "cap-blob-put: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, `{"status":"ok"}`)
}

func runCapBlobGet() {
	cap, key := "", ""
	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cap":
			if i+1 < len(args) {
				cap = args[i+1]
				i++
			}
		case "--key":
			if i+1 < len(args) {
				key = args[i+1]
				i++
			}
		case "-h", "--help":
			printCapBlobGetHelp()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			os.Exit(2)
		}
	}
	if cap == "" || key == "" {
		fmt.Fprintln(os.Stderr, "cap-blob-get: --cap and --key are required")
		printCapBlobGetHelp()
		os.Exit(2)
	}
	store, done, err := dialCapStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cap-blob-get: %v\n", err)
		os.Exit(1)
	}
	defer done()
	if err := capblob.Get(store, cap, key, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "cap-blob-get: %v\n", err)
		os.Exit(1)
	}
}

func printCapBlobPutHelp() {
	fmt.Fprintln(os.Stderr, `yesmem cap-blob-put --cap NAME --key KEY
  Reads stdin and stores it as chunked blob in the named cap's blobs table.
  Use with: curl URL | yesmem cap-blob-put --cap reddit_fetch --key "url:$URL"`)
}

func printCapBlobGetHelp() {
	fmt.Fprintln(os.Stderr, `yesmem cap-blob-get --cap NAME --key KEY
  Retrieves a previously stored blob and writes it to stdout.`)
}

var _ io.Writer = os.Stdout
