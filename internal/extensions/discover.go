package extensions

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Discover scans extDir for one-level-deep manifests and applies each one to
// sup. Used at daemon startup. Errors loading individual manifests are logged
// and surfaced via Supervisor.MarkInvalid; only catastrophic errors (e.g.
// extDir cannot be created) are returned.
func Discover(extDir string, sup *Supervisor, logger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}
	if err := os.MkdirAll(extDir, 0700); err != nil {
		return fmt.Errorf("creating %s: %w", extDir, err)
	}
	entries, err := os.ReadDir(extDir)
	if err != nil {
		return fmt.Errorf("reading %s: %w", extDir, err)
	}
	for _, e := range entries {
		// Follow symlinks: e.IsDir() returns false for symlinked directories,
		// but symlinks are a useful pattern for in-place editing of an example
		// extension during development.
		entry := filepath.Join(extDir, e.Name())
		fi, err := os.Stat(entry)
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			continue
		}
		path := filepath.Join(entry, ManifestFilename)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue // a directory without a manifest is just a state dir
		} else if err != nil {
			logger.Printf("bigband: skipping %s: %v", path, err)
			continue
		}
		applyManifestPath(sup, path, logger)
	}
	return nil
}

// applyManifestPath loads and applies a single manifest, recording invalid
// ones via the supervisor so they show up in `bigband ext list`.
func applyManifestPath(sup *Supervisor, path string, logger *log.Logger) {
	m, err := LoadManifest(path)
	if err != nil {
		// Use the parent dir name as the invalid entry's name when the
		// manifest itself can't be loaded.
		name := filepath.Base(filepath.Dir(path))
		logger.Printf("bigband: %v", err)
		sup.MarkInvalid(name, path, err.Error())
		return
	}
	sup.Apply(m)
}

// Watch starts an fsnotify-driven reload loop on extDir. It watches the
// directory itself for create/remove of subdirectories, and when a new
// manifest.yaml appears (in a freshly-added subdir, or as an edit) it
// applies it. Returns a stop function that cancels the watcher.
//
// Mirrors the shape of internal/config/watcher.go: 300ms debounce,
// best-effort error logging.
func Watch(extDir string, sup *Supervisor, logger *log.Logger) (stop func(), err error) {
	if logger == nil {
		logger = log.Default()
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := w.Add(extDir); err != nil {
		w.Close()
		return nil, err
	}
	// Watch each existing subdir of extDir so we catch manifest.yaml
	// creations *inside* an already-existing extension directory (the user
	// running `bigband-slack init` writes the manifest into a pre-existing
	// folder, which doesn't fire a Create on the parent extDir watch).
	// Symlinks resolved via os.Stat so symlinked example dirs work.
	tracked := newTrackedSet()
	if entries, err := os.ReadDir(extDir); err == nil {
		for _, e := range entries {
			subdir := filepath.Join(extDir, e.Name())
			fi, err := os.Stat(subdir)
			if err != nil || !fi.IsDir() {
				continue
			}
			if err := w.Add(subdir); err != nil {
				logger.Printf("bigband: cannot watch %s: %v", subdir, err)
			}
			mp := filepath.Join(subdir, ManifestFilename)
			if _, err := os.Stat(mp); err == nil {
				if err := w.Add(mp); err == nil {
					tracked.add(mp)
				}
			}
		}
	}

	done := make(chan struct{})
	go runWatchLoop(w, extDir, sup, tracked, logger, done)
	return func() { close(done); _ = w.Close() }, nil
}

// runWatchLoop is the long-running goroutine for Watch. Split out so the
// watcher setup stays linear in Watch and this can be tested in isolation.
func runWatchLoop(w *fsnotify.Watcher, extDir string, sup *Supervisor, tracked *trackedSet, logger *log.Logger, done <-chan struct{}) {
	const debounce = 300 * time.Millisecond
	pending := map[string]*time.Timer{}
	var pendingMu sync.Mutex

	schedule := func(path string, fn func()) {
		pendingMu.Lock()
		defer pendingMu.Unlock()
		if t, ok := pending[path]; ok {
			t.Stop()
		}
		pending[path] = time.AfterFunc(debounce, func() {
			pendingMu.Lock()
			delete(pending, path)
			pendingMu.Unlock()
			fn()
		})
	}

	for {
		select {
		case <-done:
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			handleEvent(ev, w, extDir, sup, tracked, logger, schedule)
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			logger.Printf("bigband: extensions watcher error: %v", err)
		}
	}
}

// handleEvent classifies a single fsnotify event and decides whether to
// schedule a manifest load or a removal.
func handleEvent(ev fsnotify.Event, w *fsnotify.Watcher, extDir string, sup *Supervisor, tracked *trackedSet, logger *log.Logger, schedule func(string, func())) {
	// Two cases:
	//  (a) ev.Name is the parent extDir (edit on a *file* under it would not
	//      fire here because we add child watches for that). This branch sees
	//      subdirectory create/remove and possibly Create on a manifest path
	//      directly when the user touches it as a sibling of an existing dir.
	//  (b) ev.Name is a tracked manifest file (write/rename/remove).
	if tracked.has(ev.Name) {
		// Edits to an existing manifest.
		if ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create) {
			schedule(ev.Name, func() {
				applyManifestPath(sup, ev.Name, logger)
			})
			return
		}
		if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
			tracked.remove(ev.Name)
			_ = w.Remove(ev.Name)
			name := filepath.Base(filepath.Dir(ev.Name))
			schedule(ev.Name, func() {
				logger.Printf("bigband: extension manifest removed: %s", ev.Name)
				sup.Remove(name)
			})
			return
		}
		return
	}
	// New entry under extDir (a subdir was created or symlinked in).
	if filepath.Dir(ev.Name) == extDir {
		if ev.Has(fsnotify.Create) {
			// Watch the new subdir so a later `manifest.yaml` write inside it
			// fires too. Then probe for a manifest immediately in case it was
			// created together with the dir.
			fi, err := os.Stat(ev.Name)
			if err == nil && fi.IsDir() {
				if err := w.Add(ev.Name); err != nil {
					logger.Printf("bigband: cannot watch %s: %v", ev.Name, err)
				}
			}
			candidate := filepath.Join(ev.Name, ManifestFilename)
			schedule(candidate, func() {
				if _, err := os.Stat(candidate); err == nil {
					if err := w.Add(candidate); err != nil {
						logger.Printf("bigband: cannot watch %s: %v", candidate, err)
					} else {
						tracked.add(candidate)
					}
					applyManifestPath(sup, candidate, logger)
				}
			})
		}
		if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
			candidate := filepath.Join(ev.Name, ManifestFilename)
			if tracked.has(candidate) {
				tracked.remove(candidate)
				_ = w.Remove(candidate)
				name := filepath.Base(ev.Name)
				schedule(candidate, func() {
					sup.Remove(name)
				})
			}
		}
		return
	}
	// Event inside a watched subdir — most likely a manifest.yaml appearing
	// in a previously empty extension directory (e.g. the user ran
	// `bigband-slack init` after the daemon was already up).
	if filepath.Base(ev.Name) == ManifestFilename && ev.Has(fsnotify.Create) {
		schedule(ev.Name, func() {
			if _, err := os.Stat(ev.Name); err == nil {
				if err := w.Add(ev.Name); err == nil {
					tracked.add(ev.Name)
				}
				applyManifestPath(sup, ev.Name, logger)
			}
		})
	}
}

// trackedSet is a tiny mutex-protected set of paths the watcher has explicit
// file-level watches for. Lets handleEvent disambiguate parent-directory
// events from manifest edits.
type trackedSet struct {
	mu sync.Mutex
	m  map[string]struct{}
}

func newTrackedSet() *trackedSet { return &trackedSet{m: map[string]struct{}{}} }

func (t *trackedSet) add(p string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m[p] = struct{}{}
}

func (t *trackedSet) remove(p string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, p)
}

func (t *trackedSet) has(p string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.m[p]
	return ok
}
