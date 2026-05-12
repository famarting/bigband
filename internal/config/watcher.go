package config

import (
	"log"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WatchFunc is called with a freshly loaded config on each valid change.
type WatchFunc func(*Config)

// Watch starts an fsnotify watcher on path and calls fn after a debounce
// period whenever the file changes. Blocks until the context is cancelled
// via the returned stop function.
//
// Many editors (vim, emacs, etc.) use atomic rename saves: they write to a
// temp file then rename it over the original. This changes the inode, causing
// fsnotify to lose the watch. Watch handles this by re-adding a watch on the
// original path whenever it receives a Remove event for that path, so the new
// inode is tracked and subsequent writes are caught.
func Watch(path string, fn WatchFunc) (stop func(), err error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := w.Add(path); err != nil {
		w.Close()
		return nil, err
	}

	reload := func() {
		cfg, err := Load(path)
		if err != nil {
			log.Printf("bigband: config reload error: %v", err)
			return
		}
		fn(cfg)
	}

	done := make(chan struct{})
	go func() {
		var timer *time.Timer
		defer w.Close()
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create) {
					if timer != nil {
						timer.Stop()
					}
					timer = time.AfterFunc(300*time.Millisecond, reload)
				}
				// Many editors (vim, emacs) save via atomic rename: write to a temp
				// file then rename it over the original. The rename causes the watched
				// inode to disappear, so fsnotify fires Remove and stops watching the
				// path. Re-add the watch so the new inode is tracked and the next write
				// is still caught.
				if ev.Has(fsnotify.Remove) && ev.Name == path {
					time.Sleep(20 * time.Millisecond)
					if err := w.Add(path); err != nil {
						log.Printf("bigband: config watcher: failed to re-add watch after rename: %v", err)
					} else {
						reload()
					}
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Printf("bigband: fsnotify error: %v", err)
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }, nil
}
