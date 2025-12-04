package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// LogWatcher watches session log files for changes using fsnotify
// When a log file is modified, it triggers a callback with the session name
type LogWatcher struct {
	watcher  *fsnotify.Watcher
	logDir   string
	callback func(sessionName string)
	done     chan struct{}
	mu       sync.Mutex
}

// NewLogWatcher creates a new log file watcher
// callback is called with the session name when its log file changes
func NewLogWatcher(logDir string, callback func(sessionName string)) (*LogWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	// Ensure log directory exists
	if err := os.MkdirAll(logDir, 0755); err != nil {
		w.Close()
		return nil, err
	}

	// Watch the log directory
	if err := w.Add(logDir); err != nil {
		w.Close()
		return nil, err
	}

	return &LogWatcher{
		watcher:  w,
		logDir:   logDir,
		callback: callback,
		done:     make(chan struct{}),
	}, nil
}

// Start begins watching for file changes (blocking)
// Call this in a goroutine
func (lw *LogWatcher) Start() {
	for {
		select {
		case <-lw.done:
			return
		case event, ok := <-lw.watcher.Events:
			if !ok {
				return
			}
			// Care about write and create events
			if event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create {
				// Extract session name from filename
				filename := filepath.Base(event.Name)
				if strings.HasSuffix(filename, ".log") {
					sessionName := strings.TrimSuffix(filename, ".log")
					lw.callback(sessionName)
				}
			}
		case _, ok := <-lw.watcher.Errors:
			if !ok {
				return
			}
			// Log errors but continue watching
		}
	}
}

// Close stops the watcher
func (lw *LogWatcher) Close() error {
	close(lw.done)
	return lw.watcher.Close()
}
