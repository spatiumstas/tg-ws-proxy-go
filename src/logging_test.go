package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDomainCensorWriter(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"wss://worker.sub.example.com/apiws?dst=192.0.2.1", "wss://wor***.s**.exa****.com/apiws?dst=192.0.2.1"},
		{"dial tcp: lookup example.com: no such host", "dial tcp: lookup exa****.com: no such host"},
		{"kws2.web.telegram.org /opt/var/log/tg-ws-proxy.log.1 192.0.2.1", "kws2.web.telegram.org /opt/var/log/tg-ws-proxy.log.1 192.0.2.1"},
		{"UPPER.EXAMPLE.COM up=1024 down=4096", "UP***.EXA****.COM up=1024 down=4096"},
	} {
		var out bytes.Buffer
		n, err := (domainCensorWriter{out: &out}).Write([]byte(tc.in))
		if err != nil || n != len(tc.in) || out.String() != tc.want {
			t.Errorf("mask(%q) = %q, n=%d err=%v; want %q", tc.in, out.String(), n, err, tc.want)
		}
	}
}

func TestRotatingWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// maxMB tiny -> clamped to 32KB minimum.
	w := newRotatingWriter(f, path, 0.001, 2)
	defer func() { _ = w.f.Close() }() // release handle so Windows TempDir cleanup can unlink

	line := make([]byte, 2000)
	for i := 0; i < 200; i++ { // ~400KB, rotation checked every 32 writes
		if _, err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected rotated backup %s.1 to exist: %v", path, err)
	}
	// Current log file must still exist and be smaller than total written.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() >= 400000 {
		t.Errorf("active log not rotated, size=%d", st.Size())
	}
}
