package main

import (
	"net/http"
	"sync/atomic"
	"time"
)

func debugWSDial(cfg *Config, route string, resp *http.Response, err error, elapsed time.Duration) {
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	debugf(cfg, "%s WS dial: http_status=%d elapsed=%s error=%v", route, status, elapsed.Round(time.Millisecond), err)
}

type wsProgress struct {
	up, down, lastUp, lastDown atomic.Int64
}

func observeWSProgress(cfg *Config, label string, dc int, media bool, p *wsProgress, stop <-chan struct{}, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	if !cfg.Verbose {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var reported int64
		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C:
				lastDown := p.lastDown.Load()
				if now.Sub(time.Unix(0, lastDown)) < interval || reported == lastDown {
					continue
				}
				reported = lastDown
				debugf(cfg, "[%s] DC%d media=%t WS no incoming data: last_client_read=%s last_upstream_read=%s up=%d down=%d (observation only)",
					label, dc, media, now.Sub(time.Unix(0, p.lastUp.Load())).Round(time.Second), now.Sub(time.Unix(0, lastDown)).Round(time.Second), p.up.Load(), p.down.Load())
			}
		}
	}()
	return done
}
