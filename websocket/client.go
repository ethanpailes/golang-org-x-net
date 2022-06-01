// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// DialError is an error that occurs while dialling a websocket server.
type DialError struct {
	*Config
	Err error
}

func (e *DialError) Error() string {
	return "websocket.Dial " + e.Config.Location.String() + ": " + e.Err.Error()
}

// NewConfig creates a new WebSocket config for client connection.
func NewConfig(server, origin string) (config *Config, err error) {
	config = new(Config)
	config.Version = ProtocolVersionHybi13
	config.Location, err = url.ParseRequestURI(server)
	if err != nil {
		return
	}
	config.Origin, err = url.ParseRequestURI(origin)
	if err != nil {
		return
	}
	config.Header = http.Header(make(map[string][]string))
	return
}

// NewClient creates a new WebSocket client connection over rwc.
func NewClient(config *Config, rwc io.ReadWriteCloser) (ws *Conn, err error) {
	br := bufio.NewReader(rwc)
	bw := bufio.NewWriter(rwc)
	err = hybiClientHandshake(config, br, bw)
	if err != nil {
		return
	}
	buf := bufio.NewReadWriter(br, bw)
	ws = newHybiClientConn(config, buf, rwc)
	return
}

// Dial opens a new client connection to a WebSocket.
func Dial(url_, protocol, origin string) (ws *Conn, err error) {
	config, err := NewConfig(url_, origin)
	if err != nil {
		return nil, err
	}
	if protocol != "" {
		config.Protocol = []string{protocol}
	}
	return DialConfig(config)
}

var portMap = map[string]string{
	"ws":  "80",
	"wss": "443",
}

func parseAuthority(location *url.URL) string {
	if _, ok := portMap[location.Scheme]; ok {
		if _, _, err := net.SplitHostPort(location.Host); err != nil {
			return net.JoinHostPort(location.Host, portMap[location.Scheme])
		}
	}
	return location.Host
}

// DialConfig opens a new client connection to a WebSocket with a config.
func DialConfig(config *Config) (ws *Conn, err error) {
	if config.Location == nil {
		return nil, &DialError{config, ErrBadWebSocketLocation}
	}
	if config.Origin == nil {
		return nil, &DialError{config, ErrBadWebSocketOrigin}
	}

	if config.HTTP2Transport != nil {
		return dialHTTP2(config)
	}

	return dialHTTP1(config)
}

func dialHTTP1(config *Config) (ws *Conn, err error) {
	var client net.Conn
	dialer := config.Dialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	client, err = dialWithDialer(dialer, config)
	if err != nil {
		goto Error
	}
	ws, err = NewClient(config, client)
	if err != nil {
		client.Close()
		goto Error
	}
	return

Error:
	return nil, &DialError{config, err}
}

func dialHTTP2(config *Config) (ws *Conn, err error) {
	// Respect tls config set on the top level config if the transport doesn't
	// already have one set.
	if config.TlsConfig != nil && config.HTTP2Transport.TLSClientConfig == nil {
		config.HTTP2Transport.TLSClientConfig = config.TlsConfig
	}

	// try to respect the dialer configured in the websocket config
	if config.Dialer != nil && config.HTTP2Transport.DialTLS == nil {
		config.HTTP2Transport.DialTLS = func(network, addr string, cfg *tls.Config) (net.Conn, error) {
			d := tls.Dialer{NetDialer: config.Dialer, Config: cfg}
			return d.Dial(network, addr)
		}
	}

	if config.Location.Scheme == "ws" && !config.HTTP2Transport.AllowHTTP {
		return nil, &DialError{Config: config, Err: errors.New("HTTP/2 requires TLS")}
	}

	if config.Version != ProtocolVersionHybi13 {
		return nil, &DialError{Config: config, Err: ErrBadProtocolVersion}
	}

	// https://datatracker.ietf.org/doc/html/rfc8441#section-5
	// 'The scheme of the target URI (Section 5.1 of [RFC7230]) MUST be
	//  "https" for "wss"-schemed WebSockets and "http" for "ws"-schemed
	//  WebSockets.'
	if config.Location.Scheme == "wss" {
		config.Location.Scheme = "https"
	}
	if config.Location.Scheme == "ws" {
		config.Location.Scheme = "http"
	}

	// TODO(ethan): replace pipe with something context cancelable
	sr, sw := io.Pipe()
	req, err := http.NewRequest("CONNECT", config.Location.String(), sr)
	if err != nil {
		return nil, &DialError{Config: config, Err: err}
	}

	req.Header.Add("Hack-Http2-Protocol", "websocket")
	req.Header.Add("Origin", config.Origin.String())
	req.Header.Add("Sec-Websocket-Version", fmt.Sprintf("%d", config.Version))
	if len(config.Protocol) > 0 {
		req.Header.Add("Sec-Websocket-Protocol", strings.Join(config.Protocol, ","))
	}

	// inject user supplied headers, if any
	for k, vals := range config.Header {
		req.Header[k] = vals
	}

	resp, err := config.HTTP2Transport.RoundTrip(req)
	if err != nil {
		return nil, &DialError{Config: config, Err: err}
	}

	// check response headers and status

	if resp.Header.Get("Sec-WebSocket-Extensions") != "" {
		// we don't support any extentions
		return nil, &DialError{Config: config, Err: ErrUnsupportedExtensions}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &DialError{Config: config, Err: ErrBadStatus}
	}

	// TODO(ethan): this logic is copied from the HTTP/1.1 branch.
	//              I should refactor to consolidate.
	offeredProtocol := resp.Header.Get("Sec-WebSocket-Protocol")
	if offeredProtocol != "" {
		protocolMatched := false
		for i := 0; i < len(config.Protocol); i++ {
			if config.Protocol[i] == offeredProtocol {
				protocolMatched = true
				break
			}
		}
		if !protocolMatched {
			return nil, &DialError{Config: config, Err: ErrBadWebSocketProtocol}
		}
		config.Protocol = []string{offeredProtocol}
	}

	// The handshake is complete, so we wrap things up in a Conn and return.
	stream := newHTTP2ClientStream(sw, resp)
	buf := bufio.NewReadWriter(bufio.NewReader(stream), bufio.NewWriter(stream))
	conn := newHybiClientConn(config, buf, stream)

	return conn, nil
}
