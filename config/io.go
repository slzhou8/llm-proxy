package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// writeFile persists the config atomically: it writes to a temp file in the same
// directory, fsyncs it, then renames over the target. A crash can therefore only
// leave either the old file or the new one intact, never a truncated mix.
//
// This matters because config.json holds the only copy of the upstream keys,
// client keys and account hashes, and the dashboard rewrites it on every edit.
// The previous contents are kept alongside as <name>.bak for manual recovery.
//
// Perms are 0600 throughout: the file contains API keys.
func writeFile(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	// Keep the last known-good copy before replacing it. A missing target (first
	// run) is not an error; a failed backup is not fatal either, since the write
	// below is atomic on its own.
	if prev, err := os.ReadFile(path); err == nil {
		if err := writeSync(path+".bak", prev); err != nil {
			return fmt.Errorf("backup config: %w", err)
		}
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	// On any failure past this point the temp file must not be left behind.
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once the rename below succeeded
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}
	// fsync before rename: rename only guarantees atomicity of the directory
	// entry, not that the bytes reached disk.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}

	// os.Rename replaces an existing file atomically on both POSIX and Windows.
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// writeSync writes data to path and flushes it to disk before returning.
func writeSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
