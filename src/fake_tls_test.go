package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"reflect"
	"testing"
	"time"
)

func TestFakeTLSFramingRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	wc := &fakeTLSConn{raw: a}
	rc := &fakeTLSConn{raw: b}

	payload := make([]byte, 40000) // exceeds one TLS record (16384) -> multiple records
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	go func() {
		_, _ = wc.Write(payload)
		_ = a.Close()
	}()

	got := make([]byte, 0, len(payload))
	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("framing round-trip mismatch: got %d bytes want %d", len(got), len(payload))
	}
}

func TestVerifyFakeTLSClientHello(t *testing.T) {
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}

	data := make([]byte, 76)
	data[0] = tlsRecordHandshake
	data[5] = 0x01  // ClientHello
	data[43] = 0x20 // session id length = 32
	if _, err := rand.Read(data[tlsSessionIDOffset : tlsSessionIDOffset+tlsSessionIDLen]); err != nil {
		t.Fatal(err)
	}

	// HMAC is computed over the record with the client-random region zeroed.
	zeroed := make([]byte, len(data))
	copy(zeroed, data)
	for i := 0; i < tlsClientRandomLen; i++ {
		zeroed[tlsClientRandomOffset+i] = 0
	}
	expected := hmacSHA256(secret, zeroed)

	copy(data[tlsClientRandomOffset:tlsClientRandomOffset+28], expected[:28])
	var tb [4]byte
	binary.LittleEndian.PutUint32(tb[:], uint32(time.Now().Unix()))
	for i := 0; i < 4; i++ {
		data[tlsClientRandomOffset+28+i] = tb[i] ^ expected[28+i]
	}

	cr, sid, ok := verifyFakeTLSClientHello(data, secret)
	if !ok {
		t.Fatal("expected valid fake-TLS ClientHello")
	}
	if !bytes.Equal(cr, data[tlsClientRandomOffset:tlsClientRandomOffset+tlsClientRandomLen]) {
		t.Error("client random mismatch")
	}
	if !bytes.Equal(sid, data[tlsSessionIDOffset:tlsSessionIDOffset+tlsSessionIDLen]) {
		t.Error("session id mismatch")
	}

	// Tampering the HMAC region must fail verification.
	data[tlsClientRandomOffset] ^= 0xFF
	if _, _, ok := verifyFakeTLSClientHello(data, secret); ok {
		t.Error("tampered ClientHello must be rejected")
	}
}

func TestFakeTLSServerHelloStructure(t *testing.T) {
	sessionID := bytes.Repeat([]byte{0x55}, 32)
	out, err := buildFakeTLSServerHello(make([]byte, 16), make([]byte, 32), sessionID, fakeTLSClientHelloInfo{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	next := 5 + int(binary.BigEndian.Uint16(out[3:5]))
	want := []byte{0x14, 0x03, 0x03, 0x00, 0x01, 0x01, 0x17, 0x03, 0x03}
	if next+len(want) > len(out) || !bytes.Equal(out[next:next+len(want)], want) {
		t.Fatalf("invalid CCS and application data headers at byte %d", next)
	}
	if out[43] != 32 || !bytes.Equal(out[44:76], sessionID) {
		t.Error("server hello must echo the 32-byte session ID")
	}
}

func TestWrapFakeTLSRecordsStructure(t *testing.T) {
	out := wrapFakeTLSRecords(bytes.Repeat([]byte{0xAA}, 5))
	if len(out) != 5+5 {
		t.Fatalf("wrapped len = %d, want 10 (5 header + 5 payload)", len(out))
	}
	if out[0] != tlsRecordAppData || out[1] != 0x03 || out[2] != 0x03 {
		t.Error("bad record header")
	}
	if int(binary.BigEndian.Uint16(out[3:5])) != 5 {
		t.Error("bad record length field")
	}
	if wrapFakeTLSRecords(nil) != nil {
		t.Error("empty input should produce nil")
	}
}

func fakeTLSClientHelloForTest(suites []uint16, shares map[uint16]int) []byte {
	data := make([]byte, 76)
	data[0], data[1], data[2], data[5] = 0x16, 0x03, 0x01, 0x01
	data[9], data[10], data[43] = 0x03, 0x03, 32
	copy(data[44:], bytes.Repeat([]byte{0x55}, 32))
	data = binary.BigEndian.AppendUint16(data, uint16(2*len(suites)))
	for _, suite := range suites {
		data = binary.BigEndian.AppendUint16(data, suite)
	}
	data = append(data, 1, 0)
	var keys []byte
	for _, group := range []uint16{tlsGroupX25519MLKEM768, tlsGroupX25519} {
		if size, ok := shares[group]; ok {
			keys = binary.BigEndian.AppendUint16(keys, group)
			keys = binary.BigEndian.AppendUint16(keys, uint16(size))
			keys = append(keys, make([]byte, size)...)
		}
	}
	data = binary.BigEndian.AppendUint16(data, uint16(6+len(keys)+7))
	data = append(data, 0x00, 0x33)
	data = binary.BigEndian.AppendUint16(data, uint16(2+len(keys)))
	data = binary.BigEndian.AppendUint16(data, uint16(len(keys)))
	data = append(data, keys...)
	data = append(data, 0x00, 0x2b, 0x00, 0x03, 0x02, 0x03, 0x04)
	binary.BigEndian.PutUint16(data[3:5], uint16(len(data)-5))
	binary.BigEndian.PutUint16(data[7:9], uint16(len(data)-9))
	return data
}

func readFakeTLSRecordForTest(r io.Reader) ([]byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	record := append(header, make([]byte, int(binary.BigEndian.Uint16(header[3:5])))...)
	_, err := io.ReadFull(r, record[5:])
	return record, err
}

func TestParseFakeTLSClientHello(t *testing.T) {
	suites := []uint16{0x1301, 0x1302, 0x1303, 0xc02b}
	shares := map[uint16]int{tlsGroupX25519MLKEM768: 1216, tlsGroupX25519: 32}
	hello := fakeTLSClientHelloForTest(suites, shares)
	info := parseFakeTLSClientHello(hello)
	if !reflect.DeepEqual(info.cipherSuites, suites) || !reflect.DeepEqual(info.keyShares, shares) {
		t.Fatalf("parsed ClientHello = %+v", info)
	}
	for end := 0; end < len(hello); end++ {
		info := parseFakeTLSClientHello(hello[:end])
		if len(info.cipherSuites) != 0 || len(info.keyShares) != 0 {
			t.Fatalf("accepted truncated ClientHello at %d", end)
		}
	}
	extOffset := 76 + 2 + 2*len(suites) + 2 + 2
	for _, tc := range []struct {
		name   string
		offset int
		value  uint16
	}{
		{"odd cipher list", 76, 3},
		{"extension overflow", extOffset + 2, 0xffff},
		{"key share crosses extension", extOffset + 2, 2},
		{"share list overflow", extOffset + 4, 0xffff},
		{"key overflow", extOffset + 8, 0xffff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broken := bytes.Clone(hello)
			binary.BigEndian.PutUint16(broken[tc.offset:], tc.value)
			info := parseFakeTLSClientHello(broken)
			if len(info.cipherSuites) != 0 || len(info.keyShares) != 0 {
				t.Fatalf("malformed ClientHello returned %+v", info)
			}
		})
	}
}

func TestFakeTLSServerHelloNegotiation(t *testing.T) {
	secret, clientRandom, sessionID := bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{3}, 32)
	for _, tc := range []struct {
		name     string
		suites   []uint16
		hybrid   int
		cipher   uint16
		group    uint16
		shareLen int
	}{
		{"hybrid", []uint16{0x1301, 0x1302, 0x1303}, 1216, 0x1301, tlsGroupX25519MLKEM768, 1120},
		{"x25519", []uint16{0x1301}, 0, 0x1301, tlsGroupX25519, 32},
		{"invalid hybrid size", []uint16{0x1301}, 100, 0x1301, tlsGroupX25519, 32},
		{"chacha preferred over aes256", []uint16{0x1302, 0x1303}, 0, 0x1303, tlsGroupX25519, 32},
		{"aes256 only", []uint16{0x1302}, 0, 0x1302, tlsGroupX25519, 32},
		{"legacy fallback", nil, 0, 0x1301, tlsGroupX25519, 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shares := map[uint16]int{tlsGroupX25519: 32}
			if tc.hybrid != 0 {
				shares[tlsGroupX25519MLKEM768] = tc.hybrid
			}
			info := parseFakeTLSClientHello(fakeTLSClientHelloForTest(tc.suites, shares))
			flight, err := buildFakeTLSServerHello(secret, clientRandom, sessionID, info, 64)
			if err != nil {
				t.Fatal(err)
			}
			r := bytes.NewReader(flight)
			sh, err := readFakeTLSRecordForTest(r)
			if err != nil {
				t.Fatal(err)
			}
			if sh[5] != 2 || int(sh[6])<<16|int(sh[7])<<8|int(sh[8]) != len(sh)-9 {
				t.Fatal("incorrect ServerHello handshake length")
			}
			if sh[43] != 32 || !bytes.Equal(sh[44:76], sessionID) {
				t.Fatal("session ID was not echoed")
			}
			if got := binary.BigEndian.Uint16(sh[76:78]); got != tc.cipher {
				t.Fatalf("cipher = %x, want %x", got, tc.cipher)
			}
			if int(binary.BigEndian.Uint16(sh[79:81])) != len(sh)-81 ||
				!bytes.Equal(sh[81:87], []byte{0, 0x2b, 0, 2, 3, 4}) ||
				binary.BigEndian.Uint16(sh[87:89]) != 0x33 ||
				int(binary.BigEndian.Uint16(sh[89:91])) != 4+tc.shareLen ||
				binary.BigEndian.Uint16(sh[91:93]) != tc.group ||
				int(binary.BigEndian.Uint16(sh[93:95])) != tc.shareLen || len(sh) != 95+tc.shareLen {
				t.Fatal("incorrect ServerHello extensions or key share")
			}
			if sh[len(sh)-1]&0x80 != 0 {
				t.Fatal("X25519 public key has the high bit set")
			}
			ccs, err := readFakeTLSRecordForTest(r)
			if err != nil || !bytes.Equal(ccs, []byte{0x14, 3, 3, 0, 1, 1}) {
				t.Fatalf("invalid CCS: %x, %v", ccs, err)
			}
			app, err := readFakeTLSRecordForTest(r)
			if err != nil || app[0] != 0x17 || len(app) != 69 || r.Len() != 0 {
				t.Fatalf("expected one 64-byte ApplicationData record: %v", err)
			}
			zeroed := bytes.Clone(flight)
			clear(zeroed[11:43])
			if !bytes.Equal(flight[11:43], hmacSHA256(secret, append(bytes.Clone(clientRandom), zeroed...))) {
				t.Fatal("HMAC must cover client random and all three records")
			}
		})
	}
}

func TestFakeTLSAppDataSizes(t *testing.T) {
	for _, tc := range []struct{ profile, min, max int }{
		{0, 64, 160}, {30, 64, 64}, {3000, 3000, 3000}, {1000000, 16384, 16384},
	} {
		out, err := buildFakeTLSServerHello(make([]byte, 16), make([]byte, 32), make([]byte, 32), fakeTLSClientHelloInfo{}, tc.profile)
		if err != nil {
			t.Fatal(err)
		}
		offset := 5 + int(binary.BigEndian.Uint16(out[3:5])) + 6
		size := int(binary.BigEndian.Uint16(out[offset+3 : offset+5]))
		if size < tc.min || size > tc.max || len(out) != offset+5+size {
			t.Fatalf("profile %d produced size %d, want %d..%d", tc.profile, size, tc.min, tc.max)
		}
	}
}

func TestAcceptFakeTLSClientNegotiation(t *testing.T) {
	secret := bytes.Repeat([]byte{1}, 16)
	hello := fakeTLSClientHelloForTest([]uint16{0x1303}, map[uint16]int{tlsGroupX25519MLKEM768: 1216})
	digest := hmacSHA256(secret, hello)
	copy(hello[11:43], digest)
	timestamp := uint32(time.Now().Unix())
	for i := 0; i < 4; i++ {
		hello[39+i] ^= byte(timestamp >> (8 * i))
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	_ = b.SetDeadline(time.Now().Add(2 * time.Second))
	handshake := bytes.Repeat([]byte{0x42}, handshakeLen)
	done := make(chan bool, 1)
	go func() {
		_, hs, ok := acceptFakeTLSClient(context.Background(), a, secret, "mask.example", 3000, "test")
		done <- ok && bytes.Equal(hs, handshake)
	}()
	if _, err := b.Write(hello); err != nil {
		t.Fatal(err)
	}
	sh, err := readFakeTLSRecordForTest(b)
	if err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint16(sh[76:78]) != 0x1303 || binary.BigEndian.Uint16(sh[91:93]) != tlsGroupX25519MLKEM768 {
		t.Fatal("accept did not pass ClientHello capabilities to the builder")
	}
	if _, err := readFakeTLSRecordForTest(b); err != nil {
		t.Fatal(err)
	}
	app, err := readFakeTLSRecordForTest(b)
	if err != nil || len(app) != 3005 {
		t.Fatalf("accept did not use the site profile: length %d, error %v", len(app), err)
	}
	if _, err := b.Write(wrapFakeTLSRecords(handshake)); err != nil {
		t.Fatal(err)
	}
	if !<-done {
		t.Fatal("authenticated Fake TLS connection did not deliver MTProto handshake")
	}
}

func FuzzParseFakeTLSClientHello(f *testing.F) {
	f.Add(fakeTLSClientHelloForTest([]uint16{0x1301}, map[uint16]int{tlsGroupX25519MLKEM768: 1216, tlsGroupX25519: 32}))
	f.Add([]byte{0x16, 3, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		parseFakeTLSClientHello(data)
	})
}
