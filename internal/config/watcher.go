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
func Watch(path string, fn WatchFunc) (stop func(), err error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := w.Add(path); err != nil {
		w.Close()
		return nil, err
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
					timer = time.AfterFunc(300*time.Millisecond, func() {
						cfg, err := Load(path)
						if err != nil {
							log.Printf("bigband: config reload error: %v", err)
							return
						}
						fn(cfg)
					})
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
