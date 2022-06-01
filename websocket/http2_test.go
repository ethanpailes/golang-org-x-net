// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"crypto/tls"
	// "crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"golang.org/x/net/http2"
)

var startHTTP2ServerOnce sync.Once
var http2ServerAddr string
var http2Server *httptest.Server
func startHTTP2Server() {
	mux := http.NewServeMux()

	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		Handler(func(ws *Conn) {
			defer ws.Close()
			io.Copy(ws, ws)
		}).ServeHTTP(w, r)
	})

	http2Server = httptest.NewUnstartedServer(mux)

	// Force the http server to use our patched http2 server rather than the
	// one bundled in the stdlib.
	http2.ConfigureServer(http2Server.Config, nil)

	// tell the server to support HTTP/2 in the ALPN negotiation
	http2Server.TLS = &tls.Config{
		NextProtos:   []string{http2.NextProtoTLS},
	}

	http2Server.StartTLS()

	http2ServerAddr = http2Server.Listener.Addr().String()
}

func TestHTTP2Echo(t *testing.T) {
	startHTTP2ServerOnce.Do(startHTTP2Server)

	conn := dial(t, endpoint("/echo"))
	defer conn.Close()

	_, err := conn.Write([]byte("ping"))
	if err != nil {
		t.Fatalf("writing payload: %s", err)
	}
	buf := make([]byte, 64)
	nbytes, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("reading response: %s", err)
	}
	if string(buf[:nbytes]) != "ping" {
		t.Errorf("echo response: got %q, want 'ping'", string(buf[:nbytes]))
	}
}

func dial(t *testing.T, url string) *Conn {
	t.Helper()

	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
	}

	conf, err := NewConfig(url, "https://" + http2ServerAddr)
	if err != nil {
		t.Fatalf("creating config: %s", err)
	}
	conf.TlsConfig = tlsConf
	// TODO(ethan): test that overriding the dial function works

	// ask to use HTTP/2
	conf.HTTP2Transport = &http2.Transport{}

	conn, err := DialConfig(conf)
	if err != nil {
		t.Fatalf("dialing ws endpoint: %s", err)
	}
	return conn
}

func endpoint(path string) string {
	return "wss://" + http2ServerAddr + path
}

// TEST: websockets over HTTP with AllowHTTP set on the http2.Transport
