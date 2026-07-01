// Package apply writes the generated gobackup config to disk. The Phase-0 spike
// proved gobackup hot-reloads a rewritten file via fsnotify, but also that a
// half-written or invalid file drops it to zero models — so writes MUST be
// atomic (temp + rename) and validated first.
package apply

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// FileWriter atomically writes the config and de-duplicates against the last
// bytes it wrote, so an unchanged reconcile does not touch the file (and does
// not needlessly wake gobackup's watcher).
type FileWriter struct {
	Path string
	last []byte
}

// Apply validates the config parses as YAML, then atomically replaces the file
// if the bytes changed. Returns whether a write happened.
func (w *FileWriter) Apply(cfg []byte) (changed bool, err error) {
	if bytes.Equal(cfg, w.last) {
		return false, nil
	}
	var probe any
	if err := yaml.Unmarshal(cfg, &probe); err != nil {
		return false, fmt.Errorf("refusing to write invalid YAML: %w", err)
	}
	if err := atomicWrite(w.Path, cfg); err != nil {
		return false, err
	}
	w.last = cfg
	return true, nil
}

// atomicWrite writes to a temp file in the same directory, fsyncs, then renames
// over the target so a reader (gobackup) never observes a partial file.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".gobackup-*.yml.tmp")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename over %s: %w", path, err)
	}
	return nil
}
