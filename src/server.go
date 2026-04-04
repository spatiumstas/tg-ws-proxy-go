package main

import (
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	cfg, err := parseFlags()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	if cfg.GenSecret {
		fmt.Println(cfg.SecretHex)
		return
	}

	initLogger(cfg)
	startPprof(cfg)

	linkHost := getLinkHost(cfg.Host)
	log.Printf("INFO   %s", strings.Repeat("=", 60))
	log.Printf("INFO     Telegram MTProto WS Bridge Proxy (Go)")
	log.Printf("INFO     Listening on   %s:%d", cfg.Host, cfg.Port)
	log.Printf("INFO     Secret:        %s", cfg.SecretHex)
	log.Printf("INFO     Target DC IPs:")
	for _, item := range sortedDCMap(cfg.DCMap) {
		dc, ip := item.dc, item.ip
		log.Printf("INFO       DC%d: %s", dc, ip)
	}
	log.Printf("INFO   %s", strings.Repeat("=", 60))
	log.Printf("INFO     Connect link:")
	log.Printf("INFO       tg://proxy?server=%s&port=%d&secret=dd%s", linkHost, cfg.Port, cfg.SecretHex)
	log.Printf("INFO   %s", strings.Repeat("=", 60))

	go func() {
		for {
			time.Sleep(60 * time.Second)
			log.Printf("INFO   stats: %s", stats.summary())
		}
	}()

	warmupPool(cfg)

	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		log.Fatalf("listen error: %v", err)
	}
	defer ln.Close()

	secret, _ := hex.DecodeString(cfg.SecretHex)
	sessionsSem := make(chan struct{}, cfg.MaxConns)

	acceptBackoff := acceptBackoffMin
	tcpLn, _ := ln.(*net.TCPListener)

	for {
		if tcpLn != nil {
			_ = tcpLn.SetDeadline(time.Now().Add(acceptPollTimeout))
		}
		c, err := ln.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				acceptBackoff = acceptBackoffMin
				continue
			}
			log.Printf("WARN   accept error: %v", err)
			time.Sleep(acceptBackoff)
			acceptBackoff *= 2
			if acceptBackoff > acceptBackoffMax {
				acceptBackoff = acceptBackoffMax
			}
			continue
		}
		acceptBackoff = acceptBackoffMin
		atomic.AddInt64(&stats.connectionsTotal, 1)

		select {
		case sessionsSem <- struct{}{}:
			go func(conn net.Conn) {
				defer func() { <-sessionsSem }()
				handleClient(conn, cfg, secret)
			}(c)
		default:
			log.Printf("WARN   max concurrent sessions reached (%d), dropping %s", cfg.MaxConns, c.RemoteAddr())
			_ = c.Close()
		}
	}
}

func handleClient(client net.Conn, cfg *Config, secret []byte) {
	atomic.AddInt64(&stats.connectionsActive, 1)
	defer atomic.AddInt64(&stats.connectionsActive, -1)
	defer client.Close()
	label := client.RemoteAddr().String()

	_ = setSockOpts(client, cfg.BufKB*1024)

	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	hs := make([]byte, handshakeLen)
	if _, err := io.ReadFull(client, hs); err != nil {
		debugf(cfg, "[%s] client disconnected before handshake", label)
		return
	}
	_ = client.SetReadDeadline(time.Time{})

	hi, ok := tryHandshake(hs, secret)
	if !ok {
		atomic.AddInt64(&stats.connectionsBad, 1)
		debugf(cfg, "[%s] bad handshake", label)
		return
	}

	protoInt := protoFromTag(hi.ProtoTag)
	mediaTag := ""
	if hi.IsMedia {
		mediaTag = " media"
	}

	relayInit := generateRelayInit(hi.ProtoTag, signedDC(hi.DC, hi.IsMedia))
	cltDec, cltEnc, tgEnc, tgDec, err := buildCiphers(hi.ClientDecI, relayInit, secret)
	if err != nil {
		log.Printf("ERROR  [%s] cipher init failed: %v", label, err)
		return
	}

	if isBlacklisted(hi.DC, hi.IsMedia) {
		fallback := fallbackIP(hi.DC)
		if fallback == "" {
			log.Printf("WARN   [%s] DC%d%s WS blacklisted and no fallback", label, hi.DC, mediaTag)
			return
		}
		log.Printf("INFO   [%s] DC%d%s WS blacklisted -> TCP fallback %s:443", label, hi.DC, mediaTag, fallback)
		_ = tcpFallback(client, fallback, relayInit, cltDec, cltEnc, tgEnc, tgDec)
		return
	}

	targets, hasTarget := cfg.DCPool[hi.DC]
	if !hasTarget || len(targets) == 0 {
		fallback := fallbackIP(hi.DC)
		if fallback == "" {
			log.Printf("WARN   [%s] DC%d%s no fallback available", label, hi.DC, mediaTag)
			return
		}
		log.Printf("INFO   [%s] DC%d not in config -> TCP fallback %s:443", label, hi.DC, fallback)
		_ = tcpFallback(client, fallback, relayInit, cltDec, cltEnc, tgEnc, tgDec)
		return
	}
	primaryTarget := targets[0]

	dcW := hi.DC
	if v, ok := dcOverrides[dcW]; ok {
		dcW = v
	}
	domains := wsDomains(dcW, hi.IsMedia)
	key := dcKey{DC: hi.DC, IsMedia: hi.IsMedia}
	connectWS := func(timeout time.Duration) (*websocket.Conn, bool, bool) {
		wsFailedRedirect := false
		allRedirect := true
		for _, target := range targets {
			for _, d := range domains {
				debugf(cfg, "[%s] DC%d%s -> wss://%s/apiws via %s", label, hi.DC, mediaTag, d, target)
				conn, resp, err := wsConnect(target, []string{d}, timeout)
				if err == nil {
					allRedirect = false
					return conn, wsFailedRedirect, allRedirect
				}
				atomic.AddInt64(&stats.wsErrors, 1)
				if resp != nil && isRedirect(resp.StatusCode) {
					wsFailedRedirect = true
					warnf("[%s] DC%d%s got %d from %s via %s", label, hi.DC, mediaTag, resp.StatusCode, d, target)
					continue
				}
				allRedirect = false
				warnf("[%s] DC%d%s WS connect failed via %s: %v", label, hi.DC, mediaTag, target, err)
			}
		}
		return nil, wsFailedRedirect, allRedirect
	}
	fallbackToTCP := func(wsFailedRedirect, allRedirect bool) {
		if wsFailedRedirect && allRedirect {
			setBlacklisted(key)
			warnf("[%s] DC%d%s blacklisted for WS (all redirects)", label, hi.DC, mediaTag)
		} else {
			setCooldown(key)
		}
		fallback := fallbackIP(hi.DC)
		if fallback == "" {
			fallback = primaryTarget
		}
		log.Printf("INFO   [%s] DC%d%s -> TCP fallback to %s:443", label, hi.DC, mediaTag, fallback)
		err := tcpFallback(client, fallback, relayInit, cltDec, cltEnc, tgEnc, tgDec)
		if err == nil {
			log.Printf("INFO   [%s] DC%d%s TCP fallback closed", label, hi.DC, mediaTag)
		}
	}

	var ws *websocket.Conn
	fromPool := false
	if pooled := pool.get(cfg, key, primaryTarget, domains, &stats); pooled != nil {
		ws = pooled
		fromPool = true
		log.Printf("INFO   [%s] DC%d%s -> pool hit via %s", label, hi.DC, mediaTag, primaryTarget)
	}

	if ws == nil {
		timeout := 10 * time.Second
		if inCooldown(key) {
			timeout = 2 * time.Second
		}
		wsFailedRedirect := false
		allRedirect := true
		ws, wsFailedRedirect, allRedirect = connectWS(timeout)

		if ws == nil {
			fallbackToTCP(wsFailedRedirect, allRedirect)
			return
		}
	}

	var splitter *msgSplitter
	if ms, err := newMsgSplitter(relayInit, protoInt); err == nil {
		splitter = ms
	}

	if err := ws.WriteMessage(websocket.BinaryMessage, relayInit); err != nil {
		warnf("[%s] ws init write failed: %v", label, err)
		_ = ws.Close()
		if !fromPool {
			setCooldown(key)
			fallbackToTCP(false, false)
			return
		}

		timeout := 10 * time.Second
		if inCooldown(key) {
			timeout = 2 * time.Second
		}
		wsFailedRedirect := false
		allRedirect := true
		ws, wsFailedRedirect, allRedirect = connectWS(timeout)
		if ws == nil {
			fallbackToTCP(wsFailedRedirect, allRedirect)
			return
		}
		if err := ws.WriteMessage(websocket.BinaryMessage, relayInit); err != nil {
			warnf("[%s] ws init write failed after pool retry: %v", label, err)
			_ = ws.Close()
			setCooldown(key)
			fallbackToTCP(false, false)
			return
		}
	}

	clearCooldown(key)
	atomic.AddInt64(&stats.connectionsWS, 1)

	bridgeWS(label, hi.DC, hi.IsMedia, client, ws, cltDec, cltEnc, tgEnc, tgDec, splitter)
}
