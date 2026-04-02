package main

import (
	"fmt"
	"sync/atomic"
)

type Config struct {
	Host        string
	Port        int
	SecretHex   string
	DCMap       map[int]string
	DCPool      map[int][]string
	Verbose     bool
	BufKB       int
	PoolSize    int
	LogFile     string
	LogMaxMB    float64
	LogBackups  int
	PprofListen string
}

type Stats struct {
	connectionsTotal  int64
	connectionsActive int64
	connectionsWS     int64
	connectionsTCP    int64
	connectionsBad    int64
	wsErrors          int64
	bytesUp           int64
	bytesDown         int64
	poolHits          int64
	poolMisses        int64
}

func (s *Stats) summary() string {
	hits := atomic.LoadInt64(&s.poolHits)
	misses := atomic.LoadInt64(&s.poolMisses)
	poolTotal := hits + misses
	poolS := "n/a"
	if poolTotal > 0 {
		poolS = fmt.Sprintf("%d/%d", hits, poolTotal)
	}
	return fmt.Sprintf(
		"total=%d active=%d ws=%d tcp_fb=%d bad=%d err=%d pool=%s up=%s down=%s",
		atomic.LoadInt64(&s.connectionsTotal),
		atomic.LoadInt64(&s.connectionsActive),
		atomic.LoadInt64(&s.connectionsWS),
		atomic.LoadInt64(&s.connectionsTCP),
		atomic.LoadInt64(&s.connectionsBad),
		atomic.LoadInt64(&s.wsErrors),
		poolS,
		humanBytes(atomic.LoadInt64(&s.bytesUp)),
		humanBytes(atomic.LoadInt64(&s.bytesDown)),
	)
}

type handshakeInfo struct {
	DC         int
	IsMedia    bool
	ProtoTag   []byte
	ClientDecI []byte
}

type dcKey struct {
	DC      int
	IsMedia bool
}

type dcMapItem struct {
	dc int
	ip string
}
