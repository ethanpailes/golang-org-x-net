package websocket

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// http2Handshaker performs a websocket handshake over an HTTP/2 connection.
// It is similar to a serverHandshaker, but doesn't use quite the same
// interface due to differences in the underlying transport protocol.
type http2Handshaker struct {
	// The server's config.
	config *Config
	// The user-supplied userHandshake callback.
	userHandshake func(*Config, *http.Request) error
}

// handshake performs a handshake for an HTTP/2 connection and returns a
// websocket connection or an HTTP status code and an error. The status
// code is only valid if the error is non-nil.
func (h *http2Handshaker) handshake(w http.ResponseWriter, req *http.Request) (conn *Conn, statusCode int, err error) {
	statusCode, err = h.checkHeaders(req)
	if err != nil {
		return nil, statusCode, err
	}

	// allow the user to perform protocol negotiation
	err = h.userHandshake(h.config, req)
	if err != nil {
		return nil, http.StatusForbidden, ErrBadHandshake
	}

	// All the headers we've been sent check out, so we can write
	// a 200 response and inform the client if we have chosen a particular
	// application protocol.
	if len(h.config.Protocol) > 0 {
		w.Header().Add("Sec-Websocket-Protocol", h.config.Protocol[0])
	}
	w.WriteHeader(http.StatusOK)

	// Flush to force the status onto the wire so that clients can start
	// listening.
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, http.StatusInternalServerError, errors.New("websocket: response writer must implement flusher")
	}
	flusher.Flush()

	// to get a conn, we need a buffered readwriter, a readwritecloser, and
	// the request
	stream := newHTTP2ServerStream(w, req)
	buf := bufio.NewReadWriter(bufio.NewReader(stream), bufio.NewWriter(stream))
	conn = newHybiConn(h.config, buf, stream, req)
	return conn, 0, err
}

func (h *http2Handshaker) checkHeaders(req *http.Request) (statusCode int, err error) {
	// TODO(ethan): write tests for all of these checks
	if req.Method != "CONNECT" {
		return http.StatusMethodNotAllowed, ErrBadRequestMethod
	}

	protocol := req.Header.Get("Hack-Http2-Protocol")
	if protocol != "websocket" {
		return http.StatusBadRequest, ErrBadProtocol
	}

	// "On requests that contain the :protocol pseudo-header field, the
	// :scheme and :path pseudo-header fields of the target URI (see
	// Section 5) MUST also be included."
	if req.URL.Path == "" {
		return http.StatusBadRequest, ErrBadPath
	}

	version := req.Header.Get("Sec-Websocket-Version")
	if version == "13" {
		h.config.Version = ProtocolVersionHybi13
	} else {
		return http.StatusBadRequest, ErrBadProtocolVersion
	}

	// parse the list of request protocols
	protocolCSV := strings.TrimSpace(req.Header.Get("Sec-Websocket-Protocol"))
	if protocolCSV != "" {
		protocols := strings.Split(protocolCSV, ",")
		for i := 0; i < len(protocols); i++ {
			// It is ok to mutate Protocol like this because server takes its
			// receiver by value, not reference, so the whole thing is copied
			// for each request.
			h.config.Protocol = append(h.config.Protocol, strings.TrimSpace(protocols[i]))
		}
	}

	return 0, nil
}

//
// http2ServerStream
//

// http2ServerStream is a wrapper around a request and response writer that
// implements io.ReadWriteCloser
type http2ServerStream struct {
	w http.ResponseWriter
	flusher http.Flusher
	req *http.Request
}

func newHTTP2ServerStream(w http.ResponseWriter, req *http.Request) *http2ServerStream {
	flusher, ok := w.(http.Flusher)
	if !ok {
		panic("websocket: response writer must implement flusher")
	}

	return &http2ServerStream{
		w: w,
		flusher: flusher,
		req: req,
	}
}

func (s *http2ServerStream) Read(p []byte) (n int, err error) {
	return s.req.Body.Read(p)
}
func (s *http2ServerStream) Write(p []byte) (n int, err error) {
	n, err = s.w.Write(p)
	if err != nil {
		return n, err
	}

	// We flush every time since the main websocket code is going to wrap
	// this in a bufio.Writer and expect that when the bufio.Writer is flushed
	// the bytes actually land on the wire.
	s.flusher.Flush()

	return n, err
}
func (s *http2ServerStream) Close() error {
	return s.req.Body.Close()
}

//
// http2ClientStream
//

// http2ClientStream is a wrapper around a writer and an http response that
// implements io.ReadWriteCloser
type http2ClientStream struct {
	w *io.PipeWriter
	resp *http.Response
}

func newHTTP2ClientStream(w *io.PipeWriter, resp *http.Response) *http2ClientStream {
	return &http2ClientStream{
		w: w,
		resp: resp,
	}
}

func (s *http2ClientStream) Read(p []byte) (n int, err error) {
	return s.resp.Body.Read(p)
}
func (s *http2ClientStream) Write(p []byte) (n int, err error) {
	return s.w.Write(p)
}
func (s *http2ClientStream) Close() error {
	wErr := s.w.Close()
	rErr := s.resp.Body.Close()
	if wErr != nil && rErr != nil {
		return fmt.Errorf("client close: %s: %w", wErr, rErr)
	}
	if wErr != nil {
		return wErr
	}
	if rErr != nil {
		return rErr
	}
	return nil
}
