package apply

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileWriter_writeAndDedup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gobackup.yml")
	w := &FileWriter{Path: path}

	// First write.
	changed, err := w.Apply([]byte("models: {}\n"))
	if err != nil || !changed {
		t.Fatalf("first Apply: changed=%v err=%v", changed, err)
	}
	if b, _ := os.ReadFile(path); string(b) != "models: {}\n" {
		t.Fatalf("file content = %q", b)
	}

	// Identical bytes → no write.
	changed, err = w.Apply([]byte("models: {}\n"))
	if err != nil || changed {
		t.Fatalf("dedup Apply: changed=%v err=%v", changed, err)
	}

	// Changed bytes → write.
	changed, err = w.Apply([]byte("models:\n  a: {}\n"))
	if err != nil || !changed {
		t.Fatalf("changed Apply: changed=%v err=%v", changed, err)
	}
}

func TestFileWriter_rejectsInvalidYAMLKeepingLastGood(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gobackup.yml")
	w := &FileWriter{Path: path}

	good := []byte("models:\n  a: {}\n")
	if _, err := w.Apply(good); err != nil {
		t.Fatalf("good Apply: %v", err)
	}

	changed, err := w.Apply([]byte("models: [unclosed\n"))
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
	if changed {
		t.Error("invalid YAML must not report a write")
	}
	// The last-good file must be untouched on disk.
	if b, _ := os.ReadFile(path); string(b) != string(good) {
		t.Errorf("last-good clobbered: %q", b)
	}
}

func TestFileWriter_atomicNoTempLeftBehind(t *testing.T) {
	dir := t.TempDir()
	w := &FileWriter{Path: filepath.Join(dir, "gobackup.yml")}
	if _, err := w.Apply([]byte("models: {}\n")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "gobackup.yml" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected only gobackup.yml, got %v (temp file leaked?)", names)
	}
}
