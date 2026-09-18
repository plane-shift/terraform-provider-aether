package fakeapi

import (
	"bufio"
	"net"
	"net/http"
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
