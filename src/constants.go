package main

import "time"

const (
	handshakeLen = 64
	skipLen      = 8
	prekeyLen    = 32
	keyLen       = 32
	ivLen        = 16
	protoTagPos  = 56
	dcIdxPos     = 60

	protoAbridgedInt           = 0xEFEFEFEF
	protoIntermediateInt       = 0xEEEEEEEE
	protoPaddedIntermediateInt = 0xDDDDDDDD

	wsPoolMaxAge   = 120 * time.Second
	dcFailCooldown = 30 * time.Second
	ioIdleTimeout  = 90 * time.Second
	wsWriteTimeout = 15 * time.Second
	statsFlushBytes = 256 * 1024
	acceptPollTimeout = 1 * time.Second
	acceptBackoffMin  = 5 * time.Millisecond
	acceptBackoffMax  = 1 * time.Second
	defaultMaxConns   = 1024
	defaultCFProxyDomain = "pclead.co.uk"
	defaultCFProxyDomainsURL = "https://raw.githubusercontent.com/Flowseal/tg-ws-proxy/main/.github/cfproxy-domains.txt"
	defaultCFProxyRefreshTimeout = 10 * time.Second
)

var (
	protoTagAbridged     = []byte{0xef, 0xef, 0xef, 0xef}
	protoTagIntermediate = []byte{0xee, 0xee, 0xee, 0xee}
	protoTagSecure       = []byte{0xdd, 0xdd, 0xdd, 0xdd}

	reservedFirst = map[byte]bool{0xef: true}
	reservedStart = [][]byte{
		[]byte("HEAD"),
		[]byte("POST"),
		[]byte("GET "),
		{0xee, 0xee, 0xee, 0xee},
		{0xdd, 0xdd, 0xdd, 0xdd},
		{0x16, 0x03, 0x01, 0x02},
	}

	dcFallbackDefaults = map[int]string{
		1:   "149.154.175.50",
		2:   "149.154.167.51",
		3:   "149.154.175.100",
		4:   "149.154.167.91",
		5:   "149.154.171.5",
		203: "91.105.192.100",
	}

	dcOverrides = map[int]int{203: 2}

	cfProxyDefaultDomainPool = []string{
		defaultCFProxyDomain,
	}
)
