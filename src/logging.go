package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"regexp"
	"strings"
	"sync"
)

func initLogger(cfg *Config) {
	log.SetFlags(log.Ltime)
	log.SetOutput(domainCensorWriter{out: os.Stderr})
	if cfg.LogFile == "" {
		return
	}
	f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("WARN   failed to open log file %s: %v", cfg.LogFile, err)
		return
	}
	log.SetOutput(domainCensorWriter{out: newRotatingWriter(f, cfg.LogFile, cfg.LogMaxMB, cfg.LogBackups)})
}

var logDomainPattern = regexp.MustCompile(`(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}`)

type domainCensorWriter struct{ out io.Writer }

func (w domainCensorWriter) Write(p []byte) (int, error) {
	masked := logDomainPattern.ReplaceAllStringFunc(string(p), func(domain string) string {
		lower := strings.ToLower(domain)
		if lower == "telegram.org" || strings.HasSuffix(lower, ".telegram.org") || strings.HasSuffix(lower, ".log") {
			return domain
		}
		parts := strings.Split(domain, ".")
		for i := 0; i < len(parts)-1; i++ {
			n := len(parts[i]) / 2
			parts[i] = parts[i][:n] + strings.Repeat("*", len(parts[i])-n)
		}
		return strings.Join(parts, ".")
	})
	return w.out.Write([]byte(masked))
}

func startPprof(ctx context.Context, cfg *Config) <-chan struct{} {
	done := make(chan struct{})
	if cfg.PprofListen == "" {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		srv := &http.Server{Addr: cfg.PprofListen}
		stop := context.AfterFunc(ctx, func() { _ = srv.Close() })
		defer stop()
		log.Printf("INFO   pprof enabled on http://%s/debug/pprof/", cfg.PprofListen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("WARN   pprof server stopped: %v", err)
		}
	}()
	return done
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
	log.Printf("WARN   "+format, args...)
}

func logf(format string, args ...any) {
	log.Printf(format, args...)
}
