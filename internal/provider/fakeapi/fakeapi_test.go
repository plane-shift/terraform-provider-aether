package fakeapi

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestServeRespondsWhenTheRequestBodyCannotBeRead forces io.ReadAll(r.Body)
// to fail by promising more bytes (Content-Length) than the client actually
// sends before half-closing the connection.
//
// serve runs on a per-connection goroutine net/http's Server spawns - NOT
// the goroutine that called New(t) and owns t. testing.T documents that
// FailNow (which Fatal/Fatalf call) must run on the test's own goroutine;
// calling it from any other goroutine calls runtime.Goexit on THAT
// goroutine, which just quietly ends without ever writing an HTTP response.
// The client sees a bare connection close/EOF that names nothing, and the
// real read failure is never attributed to anything - which is what this
// test would see if serve still called t.Fatalf here.
func TestServeRespondsWhenTheRequestBodyCannotBeRead(t *testing.T) {
	s := New(t)
	conn, err := net.DialTimeout("tcp", s.srv.Listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	req := "POST /api/v1/organizations HTTP/1.1\r\n" +
		"Host: " + s.srv.Listener.Addr().String() + "\r\n" +
		"Authorization: Bearer " + s.Token() + "\r\n" +
		"Content-Length: 1000\r\n" +
		"Connection: close\r\n\r\n" +
		"short"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	// Half-close the write side: the server was promised 1000 body bytes and
	// got 5, so its next Body.Read sees EOF mid-body instead of the rest of
	// the promised bytes - this is what makes io.ReadAll(r.Body) return an
	// error. The read side stays open so we can still read back a response.
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("conn is %T, want *net.TCPConn (CloseWrite needed to half-close)", conn)
	}
	if err := tcp.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("reading the response: %v (a handler that Fatalf's/Goexits mid-request never writes one - the client just sees this)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (the body-read failure reported as a response, not silently dropped)", resp.StatusCode)
	}
	if s.BodyReadError() == nil {
		t.Fatal("BodyReadError() = nil, want the read failure recorded for the test to inspect")
	}
}

// TestServeReturns404ForPathMissingTheAPIPrefix pins a review-confirmed
// defect: serve computed the routed path with strings.TrimPrefix(r.URL.Path,
// "/api/v1"), which is a no-op (returns the string unchanged) when the
// prefix is absent instead of signalling "not stripped". A request to
// "/organizations" (no "/api/v1" prefix) then falls straight through to the
// `path == "/organizations"` case and is served as if it *had* the prefix -
// silently routing a malformed/prefix-less path instead of rejecting it.
func TestServeReturns404ForPathMissingTheAPIPrefix(t *testing.T) {
	s := New(t)
	s.AddOrg("11111111-1111-4111-8111-111111111111", "Acme", "acme")

	req, err := http.NewRequest(http.MethodGet, s.URL()+"/organizations", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.Token())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, body = %q, want 404 (a path with no /api/v1 prefix must never be routed as if it had one)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "invalid API base path") {
		t.Fatalf("body = %q, want a message naming the invalid API base path", body)
	}
}

// TestWriteJSONRespondsWithErrorWhenMarshalFails pins a review-confirmed
// defect: writeJSON called w.WriteHeader(code) BEFORE marshalling, so a value
// that fails to marshal (e.g. containing a chan) leaves the caller's status
// code on the wire with an empty/truncated body instead of surfacing a 500
// and an error body - the marshal error was silently discarded.
func TestWriteJSONRespondsWithErrorWhenMarshalFails(t *testing.T) {
	w := httptest.NewRecorder()

	WriteJSON(w, http.StatusOK, map[string]any{"bad": make(chan int)})

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (a marshal failure must not leave the caller's original status code on the wire)", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v (body=%q)", err, w.Body.String())
	}
	if _, ok := body["error"]; !ok {
		t.Fatalf("body = %v, want an \"error\" key describing the marshal failure", body)
	}
}
