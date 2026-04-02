package main

import (
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"sync"
)

func initLogger(cfg *Config) {
	log.SetFlags(log.Ltime)
	if cfg.LogFile == "" {
		return
	}
	f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("WARN   failed to open log file %s: %v", cfg.LogFile, err)
		return
	}
	log.SetOutput(newRotatingWriter(f, cfg.LogFile, cfg.LogMaxMB, cfg.LogBackups))
}

func startPprof(cfg *Config) {
	if cfg.PprofListen == "" {
		return
	}
	go func(addr string) {
		log.Printf("INFO   pprof enabled on http://%s/debug/pprof/", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Printf("WARN   pprof server stopped: %v", err)
		}
	}(cfg.PprofListen)
}

type rotatingWriter struct {
	mu        sync.Mutex
	f         *os.File
	path      string
	maxBytes  int64
	backups   int
	checkTick int
}

func newRotatingWriter(f *os.File, path string, maxMB float64, backups int) *rotatingWriter {
	maxBytes := int64(maxMB * 1024 * 1024)
	if maxBytes < 32*1024 {
		maxBytes = 32 * 1024
	}
	return &rotatingWriter{f: f, path: path, maxBytes: maxBytes, backups: backups}
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.checkTick++
	if w.checkTick%32 == 0 {
		if st, err := w.f.Stat(); err == nil && st.Size() >= w.maxBytes {
			_ = w.rotate()
		}
	}
	return w.f.Write(p)
}

func (w *rotatingWriter) rotate() error {
	_ = w.f.Close()
	if w.backups > 0 {
		for i := w.backups - 1; i >= 1; i-- {
			old := fmt.Sprintf("%s.%d", w.path, i)
			newp := fmt.Sprintf("%s.%d", w.path, i+1)
			_ = os.Rename(old, newp)
		}
		_ = os.Rename(w.path, fmt.Sprintf("%s.1", w.path))
	} else {
		_ = os.Remove(w.path)
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	w.f = f
	return nil
}

func debugf(cfg *Config, format string, args ...any) {
	if cfg.Verbose {
		log.Printf("DEBUG  "+format, args...)
	}
}

func warnf(format string, args ...any) {
	log.Printf("WARNING  "+format, args...)
}

func logf(format string, args ...any) {
	log.Printf(format, args...)
}
