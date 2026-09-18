package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBearerHeaderIsAdded(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Get("Authorization") }))
	defer srv.Close()
	c := &http.Client{Transport: New("aether_x", http.DefaultTransport)}
	if _, err := c.Get(srv.URL); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer aether_x" {
		t.Fatalf("Authorization = %q", got)
	}
}

// TestRetriesOn429HonouringRetryAfter sends Retry-After: 2 (not 0) and
// records the wait passed to the injectable sleep field: with "0", deleting
// the Retry-After parsing entirely (falling back to backoff() unconditionally)
// would not fail this test, since a zeroed/near-zero backoff also produces a
// short wait. "2" only passes if the header value was actually read AND
// actually used as the wait.
//
// This needs the `sleep func(context.Context, time.Duration) error` field the
// coder is adding to retryTransport (mirroring the existing `backoff` field)
// - it does not exist yet, so this test does not compile today. That is the
// expected RED.
func TestRetriesOn429HonouringRetryAfter(t *testing.T) {
	var waits []time.Duration
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	tr := New("t", http.DefaultTransport).(*retryTransport)
	tr.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	c := &http.Client{Transport: tr}
	resp, err := c.Get(srv.URL)
	if err != nil || resp.StatusCode != 200 || atomic.LoadInt32(&n) != 2 {
		t.Fatalf("resp=%v err=%v n=%d", resp, err, n)
	}
	if len(waits) != 1 || waits[0] != 2*time.Second {
		t.Fatalf("waits = %v, want [2s] (the Retry-After header value, actually honoured)", waits)
	}
}

// TestRetryAfterIsCapped pins a review-confirmed defect: retryAfter honours
// an integer Retry-After header VERBATIM with no upper bound, so a 429
// answering "Retry-After: 3600" makes the transport wait a full hour. Uses
// the injectable sleep field (does not exist yet - expected RED is a compile
// failure) so the assertion is on the ACTUAL wait the retry loop would use,
// not a value computed off to the side.
func TestRetryAfterIsCapped(t *testing.T) {
	var waits []time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	tr := New("t", http.DefaultTransport).(*retryTransport)
	tr.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	c := &http.Client{Transport: tr}
	_, _ = c.Get(srv.URL)
	if len(waits) == 0 {
		t.Fatal("expected at least one recorded wait")
	}
	for _, w := range waits {
		if w > 60*time.Second {
			t.Fatalf("wait = %s, want capped well below Retry-After: 3600 (an hour-long stall on a single 429)", w)
		}
	}
}

// TestRetryAfterOverflowDoesNotGoNegative pins the digit-bound defect behind
// retryAfter's `if secs, err := strconv.Atoi(...); err == nil`: Atoi/ParseInt
// on a value it CAN parse but that is still large enough overflows the
// subsequent `time.Duration(secs) * time.Second` multiplication, wrapping to
// a NEGATIVE duration.
//
// The review's suggested literal, "99999999999999999999" (~1e20), does NOT
// reproduce this: it exceeds int64's range enough that Atoi itself returns
// strconv.ErrRange, so `err == nil` is false and the multiplication is never
// reached at all (verified with a throwaway repro against parse()+Atoi
// directly). "9223372037" is the smallest value that both parses cleanly (it
// is far inside int64's range) AND overflows once multiplied by time.Second -
// the same repro produced -2562047h47m16.709551616s for it. Kept this literal
// rather than reverting to the suggested one, since the suggested one is
// already safe today and would not be RED.
func TestRetryAfterOverflowDoesNotGoNegative(t *testing.T) {
	var waits []time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "9223372037")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	tr := New("t", http.DefaultTransport).(*retryTransport)
	tr.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	c := &http.Client{Transport: tr}
	_, _ = c.Get(srv.URL)
	if len(waits) == 0 {
		t.Fatal("expected at least one recorded wait")
	}
	for _, w := range waits {
		if w < 0 {
			t.Fatalf("wait = %s, want non-negative (Retry-After overflow must not become a negative sleep)", w)
		}
		if w > 60*time.Second {
			t.Fatalf("wait = %s, want capped", w)
		}
	}
}

// TestNegativeRetryAfterFallsBackToBackoff pins a review-confirmed defect:
// retryAfter's `case secs < 0: wait = 0` throws away the already-computed
// backoff(attempt) and substitutes a hard 0, meaning a malformed/hostile
// negative Retry-After produces an INSTANT retry - the opposite of backing
// off. A negative value carries no usable wait information and should be
// treated the same as "no usable Retry-After at all": fall back to
// backoff(attempt), not to zero.
func TestNegativeRetryAfterFallsBackToBackoff(t *testing.T) {
	tr := New("t", http.DefaultTransport).(*retryTransport)
	tr.backoff = func(int) time.Duration { return 7 * time.Second }
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"-5"}}}
	wait, retry := tr.retryAfter(resp, 0, true, true)
	if !retry {
		t.Fatal("expected a 429 to be retried")
	}
	if wait != 7*time.Second {
		t.Fatalf("wait = %s, want the backoff value 7s (a negative Retry-After must not become an instant 0-wait retry)", wait)
	}
}

// stubRoundTripper lets a test control exactly what error/response each
// attempt gets, including transport-level errors an httptest server cannot
// produce on demand (a dial failure, a mid-write failure).
type stubRoundTripper struct {
	do func(attempt int, req *http.Request) (*http.Response, error)
	n  int
}

func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	s.n++
	return s.do(s.n, req)
}

// TestPOSTIsRetriedAfterADialError: a dial error PROVES no bytes reached the
// server (see the package doc comment on nothingWritten), so it is safe to
// retry even a non-idempotent POST - and the body must be re-read via
// GetBody, not replayed from an already-drained reader.
func TestPOSTIsRetriedAfterADialError(t *testing.T) {
	var bodies []string
	stub := &stubRoundTripper{}
	stub.do = func(attempt int, req *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(req.Body)
		bodies = append(bodies, string(b))
		if attempt == 1 {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}, nil
	}
	tr := New("t", stub).(*retryTransport)
	tr.backoff = func(int) time.Duration { return 0 }
	req, err := http.NewRequest(http.MethodPost, "http://example.invalid/x", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("err = %v, want the POST retried after a dial error", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if stub.n != 2 {
		t.Fatalf("attempts = %d, want 2 (one retry after a dial error)", stub.n)
	}
	if len(bodies) != 2 || bodies[0] != "payload" || bodies[1] != "payload" {
		t.Fatalf("bodies = %v, want the body re-read via GetBody on the retry", bodies)
	}
}

// TestPOSTIsNotRetriedWhenBytesMayHaveReachedTheServer covers both a generic
// transport error and a write-phase net.OpError: neither PROVES nothing was
// written, so retrying either could bill/create something twice.
func TestPOSTIsNotRetriedWhenBytesMayHaveReachedTheServer(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"generic error", errors.New("boom")},
		{"write error", &net.OpError{Op: "write", Net: "tcp", Err: errors.New("broken pipe")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stub := &stubRoundTripper{}
			stub.do = func(attempt int, req *http.Request) (*http.Response, error) {
				_, _ = io.Copy(io.Discard, req.Body)
				return nil, c.err
			}
			tr := New("t", stub).(*retryTransport)
			tr.backoff = func(int) time.Duration { return 0 }
			req, err := http.NewRequest(http.MethodPost, "http://example.invalid/x", strings.NewReader("payload"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = tr.RoundTrip(req)
			if !errors.Is(err, c.err) {
				t.Fatalf("err = %v, want %v unwrapped (no retry attempted)", err, c.err)
			}
			if stub.n != 1 {
				t.Fatalf("attempts = %d, want 1 (must not retry - the write may have partially reached the server)", stub.n)
			}
		})
	}
}

// TestNoRetryOn429WhenBodyHasNoGetBody: http.NewRequest only auto-populates
// GetBody for *bytes.Buffer/*bytes.Reader/*strings.Reader bodies. Wrapping
// the reader in io.NopCloser hides it from that type switch, so GetBody stays
// nil here - exactly the "body cannot be re-opened" case the transport must
// never replay, even for the every-method-retries-429 rule.
func TestNoRetryOn429WhenBodyHasNoGetBody(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL, io.NopCloser(strings.NewReader("payload")))
	if err != nil {
		t.Fatal(err)
	}
	if req.GetBody != nil {
		t.Fatal("test setup invalid: GetBody must be nil to exercise the no-replay path")
	}
	tr := New("t", http.DefaultTransport).(*retryTransport)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 returned untouched (no retry)", resp.StatusCode)
	}
	if atomic.LoadInt32(&n) != 1 {
		t.Fatalf("n = %d, want 1 (a body with no GetBody must never be retried, even on 429)", n)
	}
}

// TestCtxCancelledDuringBackoffReturnsTheCtxError: the injected sleep cancels
// ctx itself (simulating an unrelated shutdown/timeout firing mid-backoff)
// and returns ctx.Err() - RoundTrip must propagate that, not swallow it or
// substitute the underlying 503.
func TestCtxCancelledDuringBackoffReturnsTheCtxError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := New("t", http.DefaultTransport).(*retryTransport)
	tr.sleep = func(sctx context.Context, _ time.Duration) error {
		cancel()
		return sctx.Err()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tr.RoundTrip(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRetries5xxOnGetButNotOnPost(t *testing.T) {
	var gets, posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			atomic.AddInt32(&posts, 1)
		} else {
			atomic.AddInt32(&gets, 1)
		}
		w.WriteHeader(503)
	}))
	defer srv.Close()
	tr := New("t", http.DefaultTransport).(*retryTransport)
	tr.backoff = func(int) time.Duration { return 0 }
	c := &http.Client{Transport: tr}
	_, _ = c.Get(srv.URL)
	_, _ = c.Post(srv.URL, "application/json", nil)
	if atomic.LoadInt32(&gets) != 4 { // 1 + 3 retries
		t.Fatalf("gets = %d, want 4", gets)
	}
	if atomic.LoadInt32(&posts) != 1 {
		t.Fatalf("posts = %d, want 1 (POST is not retried on 5xx)", posts)
	}
}
