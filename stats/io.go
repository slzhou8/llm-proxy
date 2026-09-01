package stats

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeAtomic writes data to path via a temp file + fsync + rename, so a crash
// mid-write cannot leave a truncated file behind. The stats file is rewritten
// every persist tick, so a plain overwrite would make corruption likely over
// time -- and an unparseable file silently resets all historical stats on load.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once the rename succeeded
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace: %w", err)
	}
	return nil
}
