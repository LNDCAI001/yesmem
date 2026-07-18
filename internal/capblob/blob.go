package capblob

import (
	"io"

	"github.com/LNDCAI001/yesmem/internal/storage"
)

type Store interface {
	CapStoreCreateTable(capName, tableName string, columns []storage.ColumnDef) error
	CapStoreUpsert(capName, tableName string, data map[string]any) (int64, error)
	CapStoreQuery(capName, tableName, where string, args []any, limit int) ([]map[string]any, error)
	CapStoreDelete(capName, tableName, where string, args []any) (int64, error)
}

const (
	DefaultChunkSize = 20000
	TableName        = "blobs"
	QueryLimit       = 1000
)

func schema() []storage.ColumnDef {
	return []storage.ColumnDef{
		{Name: "key", Type: "TEXT"},
		{Name: "chunk_idx", Type: "INTEGER"},
		{Name: "data", Type: "TEXT"},
	}
}

func Put(s Store, capName, key string, r io.Reader, chunkSize int) error {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	if err := s.CapStoreCreateTable(capName, TableName, schema()); err != nil {
		return err
	}
	if _, err := s.CapStoreDelete(capName, TableName, "key=?", []any{key}); err != nil {
		return err
	}
	buf := make([]byte, chunkSize)
	idx := 0
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			chunk := string(buf[:n])
			if _, uerr := s.CapStoreUpsert(capName, TableName, map[string]any{
				"key":       key,
				"chunk_idx": idx,
				"data":      chunk,
			}); uerr != nil {
				return uerr
			}
			idx++
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func Get(s Store, capName, key string, w io.Writer) error {
	rows, err := s.CapStoreQuery(capName, TableName, "key=?", []any{key}, QueryLimit)
	if err != nil {
		return err
	}
	byIdx := make(map[int]string, len(rows))
	maxIdx := -1
	for _, row := range rows {
		idx, ok := row["chunk_idx"].(int64)
		if !ok {
			if f, fok := row["chunk_idx"].(float64); fok {
				idx = int64(f)
				ok = true
			}
		}
		if !ok {
			continue
		}
		data, dok := row["data"].(string)
		if !dok {
			continue
		}
		byIdx[int(idx)] = data
		if int(idx) > maxIdx {
			maxIdx = int(idx)
		}
	}
	for i := 0; i <= maxIdx; i++ {
		chunk, ok := byIdx[i]
		if !ok {
			continue
		}
		if _, werr := w.Write([]byte(chunk)); werr != nil {
			return werr
		}
	}
	return nil
}
