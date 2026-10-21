package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	log.SetOutput(domainCensorWriter{out: os.Stderr})
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	if cfg.GenSecret {
		fmt.Println(cfg.SecretHex)
		return
	}
	if cfg.PrintLink {
		linkHost := getLinkHost(cfg.Host)
		if cfg.FakeTLSDomain != "" {
			fmt.Println(fakeTLSConnectLink(linkHost, cfg.Port, cfg.SecretHex, cfg.FakeTLSDomain))
		} else {
			fmt.Printf("tg://proxy?server=%s&port=%d&secret=dd%s\n", linkHost, cfg.Port, cfg.SecretHex)
		}
		return
	}

	initLogger(cfg)
	checkFDLimit(cfg)
	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		log.Fatalf("listen error: %v", err)
	}
	defer ln.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cfg.ctx = ctx
	pprofDone := startPprof(ctx, cfg)
	refreshDone := startCFProxyDomainRefresh(ctx, cfg)

	linkHost := getLinkHost(cfg.Host)
	log.Printf("INFO   %s", strings.Repeat("=", 60))
	log.Printf("INFO     Telegram MTProto WS Bridge Proxy (Go)")
	log.Printf("INFO     Listening on   %s:%d", cfg.Host, cfg.Port)
	log.Printf("INFO     Secret:        %s", cfg.SecretHex)
	if cfg.FakeTLSDomain != "" {
		log.Printf("INFO     Fake TLS:      %s", cfg.FakeTLSDomain)
	}
	log.Printf("INFO     Target DC IPs:")
	for _, item := range sortedDCMap(cfg.DCMap) {
		dc, ip := item.dc, item.ip
		log.Printf("INFO       DC%d: %s", dc, ip)
	}
	if cfg.FallbackCFProxy {
		prio := "TCP first"
		if cfg.FallbackCFProxyPriority {
			prio = "CF first"
		}
		refreshMode := "off"
		if cfg.FallbackCFProxyRefresh && !cfg.FallbackCFProxyUserDomain && strings.TrimSpace(cfg.FallbackCFProxyDomainsURL) != "" {
			refreshMode = "hourly"
		}
		log.Printf("INFO     CF proxy:      active=%s pool=%d (%s, refresh=%s)", cfg.cfproxyActiveDomain(), cfg.cfproxyDomainPoolSize(), prio, refreshMode)
	}
	if cfg.hasCFProxyWorkerDomains() {
		log.Printf("INFO     CF worker:     %s (tried first)", strings.Join(cfg.FallbackCFProxyWorkerDomains, ", "))
	}
	log.Printf("INFO   %s", strings.Repeat("=", 60))
	log.Printf("INFO     Connect link:")
	if cfg.FakeTLSDomain != "" {
		log.Printf("INFO       %s", fakeTLSConnectLink(linkHost, cfg.Port, cfg.SecretHex, cfg.FakeTLSDomain))
	} else {
		log.Printf("INFO       tg://proxy?server=%s&port=%d&secret=dd%s", linkHost, cfg.Port, cfg.SecretHex)
	}
	log.Printf("INFO   %s", strings.Repeat("=", 60))

	statsDone := make(chan struct{})
	go func() {
		defer close(statsDone)
		ticker := time.NewTicker(statsLogInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				log.Printf("INFO   stats: %s", stats.summary())
			}
		}
	}()
	pool.warmup(cfg)
	workers.warmup(cfg)
	stopPools := context.AfterFunc(ctx, func() { pool.Close(); workers.Close() })
	defer stopPools()
	defer func() {
		cancel()
		pool.Close()
		workers.Close()
		<-statsDone
		<-refreshDone
		<-pprofDone
	}()

	if err := serve(ctx, ln, cfg); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("ERROR  server stopped: %v", err)
	}
}

func serve(ctx context.Context, ln net.Listener, cfg *Config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopAccept := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stopAccept()
	var clients sync.WaitGroup
	defer func() { cancel(); clients.Wait() }()

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
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
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
			clients.Add(1)
			go func(conn net.Conn) {
				defer clients.Done()
				stopClient := context.AfterFunc(ctx, func() { _ = conn.Close() })
				defer stopClient()
				defer func() { <-sessionsSem }()
				defer func() {
					if r := recover(); r != nil {
						_ = conn.Close()
						log.Printf("ERROR  [%s] panic recovered: %v", conn.RemoteAddr(), r)
					}
				}()
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

	handshakeConn := client
	if cfg.FakeTLSDomain != "" {
		fconn, hs, ok := acceptFakeTLSClient(cfg.context(), client, secret, cfg.FakeTLSDomain, label)
		if !ok {
			return
		}
		handshakeConn = fconn
		hi, ok := tryHandshake(hs, secret)
		if !ok {
			atomic.AddInt64(&stats.connectionsBad, 1)
			debugf(cfg, "[%s] bad handshake", label)
			return
		}
		handleMTProtoClient(handshakeConn, cfg, hi, secret, label)
		return
	}

	_ = handshakeConn.SetReadDeadline(time.Now().Add(clientHandshakeTimeout))
	hs := make([]byte, handshakeLen)
	if _, err := io.ReadFull(handshakeConn, hs); err != nil {
		debugf(cfg, "[%s] client disconnected before handshake", label)
		return
	}
	_ = handshakeConn.SetReadDeadline(time.Time{})

	hi, ok := tryHandshake(hs, secret)
	if !ok {
		atomic.AddInt64(&stats.connectionsBad, 1)
		debugf(cfg, "[%s] bad handshake", label)
		return
	}
	handleMTProtoClient(handshakeConn, cfg, hi, secret, label)
}

func handleMTProtoClient(client net.Conn, cfg *Config, hi *handshakeInfo, secret []byte, label string) {
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

	newFallbackSplitter := func() *msgSplitter { return &msgSplitter{proto: protoInt} }

	doFallback := func(primaryTarget string) {
		fallback := fallbackIP(hi.DC)
		if fallback == "" {
			fallback = primaryTarget
		}

		useWorker := cfg.hasCFProxyWorkerDomains()
		tryWorker := func() bool {
			if err := cfWorkerFallback(label, cfg, hi.DC, hi.IsMedia, fallback, client, relayInit, cltDec, cltEnc, tgEnc, tgDec); err == nil {
				log.Printf("INFO   [%s] DC%d%s CF worker fallback closed", label, hi.DC, mediaTag)
				return true
			}
			return false
		}

		useCF := cfg.FallbackCFProxy && cfg.hasCFProxyDomains()
		tryCF := func() bool {
			splitter := newFallbackSplitter()
			if err := cfproxyFallback(label, cfg, hi.DC, hi.IsMedia, client, relayInit, cltDec, cltEnc, tgEnc, tgDec, splitter); err == nil {
				log.Printf("INFO   [%s] DC%d%s CF proxy fallback closed", label, hi.DC, mediaTag)
				return true
			}
			return false
		}
		tryTCP := func() bool {
			if fallback == "" {
				return false
			}
			err := tcpFallback(cfg.context(), client, fallback, relayInit, cltDec, cltEnc, tgEnc, tgDec)
			if err == nil {
				log.Printf("INFO   [%s] DC%d%s TCP fallback closed", label, hi.DC, mediaTag)
				return true
			}
			return false
		}

		if useWorker && tryWorker() {
			return
		}

		if useCF && cfg.FallbackCFProxyPriority {
			if tryCF() || tryTCP() {
				return
			}
		} else if useCF {
			if tryTCP() || tryCF() {
				return
			}
		} else if tryTCP() {
			return
		}

		log.Printf("WARN   [%s] DC%d%s no fallback available", label, hi.DC, mediaTag)
	}

	primaryTarget := ""
	if targets := cfg.DCPool[hi.DC]; len(targets) > 0 {
		primaryTarget = targets[0]
	}
	ws := pool.getForDC(cfg, dcKey{DC: hi.DC, IsMedia: hi.IsMedia})
	if ws == nil {
		debugf(cfg, "[%s] DC%d%s WS pool unavailable -> fallback", label, hi.DC, mediaTag)
		doFallback(primaryTarget)
		return
	}
	stop := context.AfterFunc(cfg.context(), func() { _ = ws.Close() })
	err = writeWSBinary(ws, relayInit)
	stop()
	if err != nil {
		warnf("[%s] pooled WS init write failed: %v", label, err)
		_ = ws.Close()
		doFallback(primaryTarget)
		return
	}
	atomic.AddInt64(&stats.connectionsWS, 1)
	bridgeWS(label, cfg, hi.DC, hi.IsMedia, client, ws, cltDec, cltEnc, tgEnc, tgDec, newFallbackSplitter())
}
