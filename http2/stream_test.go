// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

var startHTTP2ServerOnce sync.Once
var http2ServerAddr string
var http2Server *httptest.Server
func startHTTP2Server() {
	mux := http.NewServeMux()

	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		writeFlusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "writer cannot be flushed", http.StatusInternalServerError)
			return
		}

		// Before begining any sort of streaming type behavior, we
		// need to push some response headers so the client knows
		// it is ok to start streaming.
		w.WriteHeader(http.StatusOK)
		writeFlusher.Flush()

		buf := make([]byte, 1024)
		for {
			nbytes, err := r.Body.Read(buf)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			_, err = w.Write(buf[:nbytes])
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			writeFlusher.Flush()
		}
	})

	http2Server = httptest.NewUnstartedServer(mux)

	// Force the http server to use our patch http2 server rather than
	// the one bundled in the stdlib.
	ConfigureServer(http2Server.Config, nil)

	// tell the server to support HTTP/2 in the ALPN negotiation
	http2Server.TLS = &tls.Config{
		NextProtos:   []string{NextProtoTLS},
	}

	http2Server.StartTLS()

	http2ServerAddr = http2Server.Listener.Addr().String()
}

func TestHTTP2Stream(t *testing.T) {
	startHTTP2ServerOnce.Do(startHTTP2Server)

	client := makeClient(t)

	// NOTE: using this idiom will mean writes are not context
	//       safe. For the real websocket code, we need to make
	//       a wrapper that allows us to cancel the writes if
	//       our context gets canceled. This is fine for a POC
	//       though.
	sr, sw := io.Pipe()
	req, err := http.NewRequest("CONNECT", endpoint("/stream"), sr)
	if err != nil {
		t.Fatal(err)
	}

	// TODO(ethan): This is a gross hack. Users shouldn't be setting
	//              psudo headers by setting things in the headers hashmap.
	//              I think the real solution here is to add a new `Protocol`
	//              field to the `http.Request` struct.
	req.Header.Add("Hack-Http2-Protocol", "websocket")

	resp, err := client.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		err = resp.Body.Close()
		if err != nil {
			t.Errorf("close resp body err: %s", err)
		}

		err = sw.Close()
		if err != nil {
			t.Errorf("close stream writer err: %s", err)
		}
	}()

	for i := 0; i < 2; i++ {
		_, err = sw.Write([]byte("ping"))
		if err != nil {
			t.Fatalf("write err: %s", err)
		}

		buf := make([]byte, 64)
		nbytes, err := resp.Body.Read(buf)
		if err != nil {
			t.Fatalf("read err: %s", err)
		}

		if string(buf[:nbytes]) != "ping" {
			t.Errorf("buf = %q, want 'ping'", string(buf[:nbytes]))
		}
	}
}

func makeClient(t *testing.T) *http.Client {
	t.Helper()

	certPool := x509.NewCertPool()
	certPool.AppendCertsFromPEM(http2Server.TLS.Certificates[0].Certificate[0])

	conf := &tls.Config{
		InsecureSkipVerify: true,
	}

	return &http.Client{
		Transport: &Transport{
			TLSClientConfig: conf,
		},
	}
}

func endpoint(path string) string {
	return "https://" + http2ServerAddr + path
}
