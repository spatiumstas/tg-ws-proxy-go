package main

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	testIOTimeout    = 2 * time.Second
	testDeliverGrace = 300 * time.Millisecond
)

func TestBridgeWSByteIntegrity(t *testing.T) {
	t.Run("packets", func(t *testing.T) { testBridgeWSByteIntegrity(t, &msgSplitter{proto: protoIntermediateInt}, false) })
	t.Run("tunnel", func(t *testing.T) { testBridgeWSByteIntegrity(t, nil, false) })
	t.Run("pooled", func(t *testing.T) { testBridgeWSByteIntegrity(t, &msgSplitter{proto: protoIntermediateInt}, true) })
}

func testBridgeWSByteIntegrity(t *testing.T, splitter *msgSplitter, pooled bool) {
	secret := make([]byte, 16)
	clientDecI := make([]byte, prekeyLen+ivLen)
	relayInit := make([]byte, handshakeLen)
	_, _ = rand.Read(secret)
	_, _ = rand.Read(clientDecI)
	_, _ = rand.Read(relayInit)

	peerCltDec, peerCltEnc, peerTgEnc, peerTgDec, err := buildCiphers(clientDecI, relayInit, secret)
	if err != nil {
		t.Fatal(err)
	}

	upPlain := buildIntermediate(8, 12, 100000)
	downPlain := []byte("downstream-payload-from-telegram")
	if pooled {
		downPlain = bytes.Repeat(downPlain, 4096)
	}
	upCh := make(chan []byte, 1)

	upgrader := websocket.Upgrader{Subprotocols: []string{"binary"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		// Receive upstream (proxy re-encrypted for Telegram) and decrypt.
		var c2 []byte
		for len(c2) < len(upPlain) {
			_, part, err := c.ReadMessage()
			if err != nil {
				return
			}
			c2 = append(c2, part...)
		}
		up := make([]byte, len(c2))
		peerTgEnc.XORKeyStream(up, c2)
		upCh <- up
		// Send downstream encrypted so the proxy's tgDec recovers it.
		c3 := make([]byte, len(downPlain))
		peerTgDec.XORKeyStream(c3, downPlain)
		if pooled {
			_ = c.WriteMessage(websocket.BinaryMessage, c3[:len(c3)/2])
			_ = c.WriteMessage(websocket.BinaryMessage, c3[len(c3)/2:])
		} else {
			_ = c.WriteMessage(websocket.BinaryMessage, c3)
		}
		time.Sleep(testDeliverGrace)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/apiws"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}

	clientProxy, clientApp := net.Pipe()
	cltDec, cltEnc, tgEnc, tgDec, err := buildCiphers(clientDecI, relayInit, secret)
	if err != nil {
		t.Fatal(err)
	}
	var upstream wsConn = ws
	if pooled {
		item := newPooledWS(ws)
		upstream = &item
	}
	go bridgeWS("test", &Config{}, 2, false, clientProxy, upstream, cltDec, cltEnc, tgEnc, tgDec, splitter)

	// Client app sends upstream (encrypted with its send stream == proxy cltDec).
	c := make([]byte, len(upPlain))
	peerCltDec.XORKeyStream(c, upPlain)
	go func() {
		_ = clientApp.SetWriteDeadline(time.Now().Add(testIOTimeout))
		_, _ = clientApp.Write(c)
	}()

	select {
	case got := <-upCh:
		if !bytes.Equal(got, upPlain) {
			t.Fatalf("upstream mismatch: got %q want %q", got, upPlain)
		}
	case <-time.After(testIOTimeout):
		t.Fatal("timeout waiting for upstream bytes at Telegram side")
	}

	// Read downstream at the client app and decrypt with its recv stream (== cltEnc).
	_ = clientApp.SetReadDeadline(time.Now().Add(testIOTimeout))
	c4 := make([]byte, len(downPlain))
	if _, err := io.ReadFull(clientApp, c4); err != nil {
		t.Fatalf("reading downstream: %v", err)
	}
	dp := make([]byte, len(c4))
	peerCltEnc.XORKeyStream(dp, c4)
	if !bytes.Equal(dp, downPlain) {
		t.Fatalf("downstream mismatch: got %q want %q", dp, downPlain)
	}

	_ = clientApp.Close()
	_ = ws.Close()
}
