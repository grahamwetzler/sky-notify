package app

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// decodeStrict decodes r as JSON onto v, rejecting any field v does not define. Shared by
// every entry point that accepts a document from outside the process — the HTTP handlers,
// CLI import, and the import document's own config/alerts sections — so all of them reject
// an unknown field the same way rather than each redeclaring the decoder.
func decodeStrict(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// readLimited reads at most limit bytes and reports an error if the source had more,
// so a truncated document is never mistaken for a complete short one.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response exceeds %d byte limit", limit)
	}
	return b, nil
}

// writeFileDurable writes atomically and durably: write, sync the file, close, rename,
// then sync the parent directory. Syncing the directory before the rename would not
// make the rename durable.
func writeFileDurable(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename succeeds

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	df, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer df.Close()
	return df.Sync()
}
