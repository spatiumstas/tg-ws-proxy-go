package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBuiltinDomainPoolIsValid(t *testing.T) {
	for _, domain := range defaultCFProxyDomains() {
		if !isValidCFProxyDomain(domain) {
			t.Fatalf("invalid builtin domain %q", domain)
		}
	}
}

func TestFetchDomainList(t *testing.T) {
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{"# comment\nvirkgj.com\nsecond.co.uk\nthird.co.uk\nsecond.co.uk", true},
		{"<html>upstream error</html>", false},
		{"one.co.uk\none.co.uk", false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("cb") == "" {
				t.Error("missing cache buster")
			}
			fmt.Fprint(w, tc.body)
		}))
		got, err := fetchCFProxyDomains(context.Background(), srv.URL, time.Second)
		srv.Close()
		if (err == nil) != tc.ok {
			t.Fatalf("fetch = %v, %v", got, err)
		}
		if tc.ok && (len(got) != 3 || got[0] != "pclead.co.uk") {
			t.Fatalf("decoded domains %v", got)
		}
	}
}

func TestNormalizeCFProxyDomain(t *testing.T) {
	cases := map[string]string{
		"  EXAMPLE.COM. ": "example.com",
		"Foo.Bar":         "foo.bar",
		".leading":        "leading",
		"":                "",
	}
	for in, want := range cases {
		if got := normalizeCFProxyDomain(in); got != want {
			t.Errorf("normalizeCFProxyDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseCFProxyDomainCSV(t *testing.T) {
	got, err := parseCFProxyDomainCSV("a.tld, b.tld , a.tld,")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"a.tld", "b.tld"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v (dedup + trim expected)", got, want)
	}
}

func TestParseCFProxyDomainCSVInvalid(t *testing.T) {
	if _, err := parseCFProxyDomainCSV("not_a_domain"); err == nil {
		t.Error("expected error for invalid domain, got nil")
	}
	if _, err := parseCFProxyDomainCSV(" , , "); err == nil {
		t.Error("expected error for empty pool, got nil")
	}
}

func TestIsValidCFProxyDomain(t *testing.T) {
	valid := []string{"foo.co.uk", "name-1234.user.workers.dev", "a.bc"}
	for _, d := range valid {
		if !isValidCFProxyDomain(d) {
			t.Errorf("isValidCFProxyDomain(%q) = false, want true", d)
		}
	}
	invalid := []string{"foo", "a..b", "-x.com", "x-.com", "1.2", ""}
	for _, d := range invalid {
		if isValidCFProxyDomain(d) {
			t.Errorf("isValidCFProxyDomain(%q) = true, want false", d)
		}
	}
}

func TestIsLikelyDomain(t *testing.T) {
	if !isLikelyDomain("a.b") {
		t.Error("a.b should be likely")
	}
	if isLikelyDomain("ab") {
		t.Error("ab (no dot) should not be likely")
	}
	if isLikelyDomain("a_b.c") {
		t.Error("underscore should not be likely")
	}
}

func TestDecodeCFProxyDomainPassthrough(t *testing.T) {
	// Non-.com domains are passed through (normalized) unchanged.
	for _, d := range []string{"example.co.uk", "kws1.web.telegram.org", "x.workers.dev"} {
		if got := decodeCFProxyDomain(d); got != d {
			t.Errorf("decodeCFProxyDomain(%q) = %q, want passthrough", d, got)
		}
	}
}

func TestDecodeCFProxyDomainComMapsToCoUk(t *testing.T) {
	got := decodeCFProxyDomain("abcde.com")
	if !strings.HasSuffix(got, ".co.uk") {
		t.Errorf("decodeCFProxyDomain(.com) = %q, want .co.uk suffix", got)
	}
}

func TestAppendUniqueDomains(t *testing.T) {
	out := appendUniqueDomains(nil, "A.tld", "a.tld", "b.tld", "")
	if strings.Join(out, ",") != "a.tld,b.tld" {
		t.Errorf("appendUniqueDomains = %v, want [a.tld b.tld]", out)
	}
}
