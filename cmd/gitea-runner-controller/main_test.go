package main

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPClientWithoutCAUsesSystemTransport(t *testing.T) {
	c, err := httpClient(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Transport == nil {
		t.Fatal("no transport")
	}
}

func TestHTTPClientRejectsInvalidCA(t *testing.T) {
	if _, err := httpClient([]byte("not a certificate")); err == nil {
		t.Fatal("expected invalid CA error")
	}
}

func TestHTTPClientTrustsCustomCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	trusted, err := httpClient(ca)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := trusted.Get(srv.URL)
	if err != nil {
		t.Fatalf("custom CA not trusted: %v", err)
	}
	_ = resp.Body.Close()

	// The same self-signed endpoint must fail without the custom CA: this proves
	// the test server is actually using a non-public certificate.
	untrusted, err := httpClient(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := untrusted.Get(srv.URL); err == nil {
		t.Fatal("self-signed endpoint unexpectedly trusted without CA")
	}
}
