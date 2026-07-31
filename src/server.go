package main

import (
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
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
	startPprof(cfg)
	startCFProxyDomainRefresh(cfg)

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
			refreshMode = "startup"
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

	go func() {
		for {
			time.Sleep(statsLogInterval)
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
		fconn, hs, ok := acceptFakeTLSClient(client, secret, cfg.FakeTLSDomain, label)
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

func splitWSTargets(targets []string, skipCooldown bool) (directTargets, frontingTargets []string) {
	frontingTargets = targets
	if !skipCooldown {
		return targets, frontingTargets
	}
	directTargets = make([]string, 0, len(targets))
	for _, target := range targets {
		if !inIPCooldown(target) {
			directTargets = append(directTargets, target)
		}
	}
	return directTargets, frontingTargets
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

	newFallbackSplitter := func() *msgSplitter {
		ms, err := newMsgSplitter(relayInit, protoInt)
		if err != nil {
			return nil
		}
		return ms
	}

	doFallback := func(setState bool, wsFailedRedirect bool, allRedirect bool, primaryTarget string) {
		key := dcKey{DC: hi.DC, IsMedia: hi.IsMedia}
		if setState {
			if wsFailedRedirect && allRedirect {
				setBlacklisted(key)
				warnf("[%s] DC%d%s blacklisted for WS (all redirects)", label, hi.DC, mediaTag)
			} else {
				setCooldown(key)
			}
		}

		fallback := fallbackIP(hi.DC)
		if fallback == "" {
			fallback = primaryTarget
		}

		useWorker := cfg.hasCFProxyWorkerDomains()
		tryWorker := func() bool {
			if err := cfWorkerFallback(label, cfg, hi.DC, hi.IsMedia, fallback, client, relayInit, cltDec, cltEnc, tgEnc, tgDec, nil); err == nil {
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
			log.Printf("INFO   [%s] DC%d%s -> TCP fallback to %s:443", label, hi.DC, mediaTag, fallback)
			err := tcpFallback(client, fallback, relayInit, cltDec, cltEnc, tgEnc, tgDec)
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

	if isBlacklisted(hi.DC, hi.IsMedia) {
		log.Printf("INFO   [%s] DC%d%s WS blacklisted -> fallback", label, hi.DC, mediaTag)
		doFallback(false, false, false, "")
		return
	}

	targets, hasTarget := cfg.DCPool[hi.DC]
	if !hasTarget || len(targets) == 0 {
		log.Printf("INFO   [%s] DC%d%s not in config -> fallback", label, hi.DC, mediaTag)
		doFallback(false, false, false, "")
		return
	}
	primaryTarget := targets[0]
	hasAnyCFFallback := cfg.hasCFProxyWorkerDomains() || (cfg.FallbackCFProxy && cfg.hasCFProxyDomains())
	directTargets, frontingTargets := splitWSTargets(targets, hasAnyCFFallback)
	wsTarget := primaryTarget
	if len(directTargets) > 0 {
		wsTarget = directTargets[0]
	}

	dcW := hi.DC
	if v, ok := dcOverrides[dcW]; ok {
		dcW = v
	}
	domains := wsDomains(dcW, hi.IsMedia)
	key := dcKey{DC: hi.DC, IsMedia: hi.IsMedia}
	connectWS := func(timeout time.Duration) (*websocket.Conn, string, bool, bool, bool) {
		wsFailedRedirect := false
		allRedirect := true
		timedOut := false
		for _, target := range directTargets {
			for _, d := range domains {
				debugf(cfg, "[%s] DC%d%s -> wss://%s/apiws via %s", label, hi.DC, mediaTag, d, target)
				conn, resp, err := dialWS(target, d, timeout)
				if err == nil {
					allRedirect = false
					return conn, target, wsFailedRedirect, allRedirect, timedOut
				}
				atomic.AddInt64(&stats.wsErrors, 1)
				if resp != nil && isRedirect(resp.StatusCode) {
					wsFailedRedirect = true
					warnf("[%s] DC%d%s got %d from %s via %s", label, hi.DC, mediaTag, resp.StatusCode, d, target)
					continue
				}
				if isTimeoutError(err) {
					timedOut = true
					allRedirect = false
					if hasAnyCFFallback {
						setIPCooldown(target)
					}
					warnf("[%s] DC%d%s WS connect timed out via %s", label, hi.DC, mediaTag, target)
					break
				}
				allRedirect = false
				warnf("[%s] DC%d%s WS connect failed via %s: %v", label, hi.DC, mediaTag, target, err)
			}
		}
		return nil, "", wsFailedRedirect, allRedirect, timedOut
	}

	connectFronting := func(reason string) (*websocket.Conn, string) {
		for _, target := range frontingTargets {
			log.Printf("INFO   [%s] DC%d%s -> fronting %s via %s", label, hi.DC, mediaTag, reason, target)
			conn, _, err := wsConnectFronting(target, domains, wsConnectTimeout)
			if err == nil {
				setFrontingActive()
				atomic.AddInt64(&stats.connectionsFront, 1)
				log.Printf("INFO   [%s] DC%d%s fronting OK for %ds", label, hi.DC, mediaTag, int(frontingCooldown.Seconds()))
				return conn, target
			}
			atomic.AddInt64(&stats.wsErrors, 1)
			warnf("[%s] DC%d%s fronting failed via %s: %v", label, hi.DC, mediaTag, target, err)
		}
		return nil, ""
	}

	dialFresh := func() (*websocket.Conn, string, bool) {
		if frontingActive() {
			if conn, target := connectFronting("active"); conn != nil {
				return conn, target, false
			}
			clearFrontingActive()
		}
		if len(directTargets) == 0 {
			log.Printf("INFO   [%s] DC%d%s WS target IPs are timed out -> fallback", label, hi.DC, mediaTag)
			doFallback(false, false, false, primaryTarget)
			return nil, "", false
		}
		timeout := wsConnectTimeout
		if inCooldown(key) {
			timeout = wsConnectCooldownTimeout
		}
		conn, target, wsFailedRedirect, allRedirect, timedOut := connectWS(timeout)
		if conn == nil {
			if timedOut {
				if conn, target := connectFronting("fallback"); conn != nil {
					return conn, target, false
				}
			}
			doFallback(true, wsFailedRedirect, allRedirect, primaryTarget)
		}
		return conn, target, conn != nil
	}

	ws := pool.get(cfg, key, wsTarget, domains)
	usedTarget := wsTarget
	directWS := false
	fromPool := ws != nil
	if fromPool {
		log.Printf("INFO   [%s] DC%d%s -> pool hit via %s", label, hi.DC, mediaTag, wsTarget)
	} else {
		ws, usedTarget, directWS = dialFresh()
		if ws == nil {
			return
		}
	}

	var splitter *msgSplitter
	if ms, err := newMsgSplitter(relayInit, protoInt); err == nil {
		splitter = ms
	}

	if err := writeWSBinary(ws, relayInit); err != nil {
		warnf("[%s] ws init write failed: %v", label, err)
		_ = ws.Close()
		if !fromPool {
			setCooldown(key)
			doFallback(false, false, false, primaryTarget)
			return
		}
		ws, usedTarget, directWS = dialFresh()
		if ws == nil {
			return
		}
		if err := writeWSBinary(ws, relayInit); err != nil {
			warnf("[%s] ws init write failed after pool retry: %v", label, err)
			_ = ws.Close()
			setCooldown(key)
			doFallback(false, false, false, primaryTarget)
			return
		}
	}

	clearCooldown(key)
	if directWS {
		clearIPCooldown(usedTarget)
	}
	atomic.AddInt64(&stats.connectionsWS, 1)

	bridgeWS(label, cfg, hi.DC, hi.IsMedia, client, ws, cltDec, cltEnc, tgEnc, tgDec, splitter)
}
