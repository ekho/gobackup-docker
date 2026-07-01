package pipeline

import (
	"context"
	"log"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

// WatchFile calls fire() whenever the file at path is written or replaced. It
// watches the file's DIRECTORY (not the inode) so an atomic rename over the file
// is still detected — the same reason gobackup's own reload survives renames.
// Blocks until ctx is cancelled.
func WatchFile(ctx context.Context, path string, fire func()) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[defaults-watch] cannot create watcher: %v", err)
		return
	}
	defer w.Close()

	dir := filepath.Dir(path)
	if err := w.Add(dir); err != nil {
		log.Printf("[defaults-watch] cannot watch %s (defaults edits won't auto-apply): %v", dir, err)
		return
	}
	target := filepath.Clean(path)

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if filepath.Clean(ev.Name) == target {
				fire()
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("[defaults-watch] %v", err)
		}
	}
}
