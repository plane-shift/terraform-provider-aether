// Package transport is the provider's HTTP layer: bearer auth plus bounded
// retries.
//
// The retry rules are asymmetric ON PURPOSE. A 5xx to a GET costs nothing to
// repeat; a 5xx to a POST may mean the cluster WAS created and the answer was
// lost, so repeating it bills the customer twice. So:
//
//   - 429 is retried for every method: the server is explicitly saying "not
//     now", which is a statement that it did not act on the request.
//   - 5xx is retried only for idempotent methods (GET/HEAD/PUT/DELETE).
//   - A transport error is retried for idempotent methods, and for the rest
//     only when the failure PROVES no bytes reached the server (a dial or DNS
//     failure). net/http exposes no "nothing written" signal of its own, so a
//     connection that broke mid-body is deliberately NOT replayed.
//   - Nothing is replayed unless the body can be re-opened via GetBody.
package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// maxRetries is the number of RETRIES, so an idempotent request makes at most
// 1 + maxRetries attempts.
const maxRetries = 3

// maxRetryAfter bounds what a server can make us wait. Retry-After is
// attacker- or bug-controlled input: an hour-long value would park a
// terraform apply on a single 429, and a large-but-parseable one overflows
// `time.Duration(secs) * time.Second` into a NEGATIVE duration. The clamp is
// applied BEFORE the multiplication, so overflow is unreachable rather than
// merely unlikely.
const maxRetryAfter = 60 * time.Second

type retryTransport struct {
	token string
	next  http.RoundTripper
	// backoff and sleep are fields rather than package functions so tests can
	// zero the one and record the other. sleep receives the request's context.
	backoff func(attempt int) time.Duration
	sleep   func(ctx context.Context, d time.Duration) error
}

// New returns an http.RoundTripper that adds the bearer token header and
// retries as described above before delegating to next.
func New(token string, next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return &retryTransport{token: token, next: next, backoff: defaultBackoff, sleep: defaultSleep}
}

// defaultBackoff is 500ms, 1s, 2s, capped at 4s.
func defaultBackoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= 3 {
		return 4 * time.Second
	}
	d := 500 * time.Millisecond << attempt
	if d > 4*time.Second {
		d = 4 * time.Second
	}
	return d
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// RoundTrip must not modify the caller's request; every mutation below is
	// on this clone.
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "terraform-provider-aether")
	}

	hasBody := req.Body != nil && req.Body != http.NoBody
	// Never replay a body that cannot be re-opened: the second attempt would
	// send an empty or half-consumed payload, which is worse than the failure.
	replayable := !hasBody || req.GetBody != nil
	idempotent := isIdempotent(req.Method)

	for attempt := 0; ; attempt++ {
		if attempt > 0 && hasBody {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = body
		}

		resp, err := t.next.RoundTrip(req)
		if err != nil {
			if attempt == maxRetries || !replayable || !(idempotent || nothingWritten(err)) {
				return nil, err
			}
			if serr := t.sleep(req.Context(), t.backoff(attempt)); serr != nil {
				return nil, err
			}
			continue
		}

		wait, retry := t.retryAfter(resp, attempt, idempotent, replayable)
		if !retry || attempt == maxRetries {
			// The caller gets this response untouched - body unread and
			// unclosed - so the final answer propagates exactly as if there
			// had been no retry layer at all.
			return resp, nil
		}
		// Discarded response: drain so the connection can be reused, and close
		// so it is not leaked.
		drain(resp)
		if err := t.sleep(req.Context(), wait); err != nil {
			return nil, err
		}
	}
}

// retryAfter decides whether resp should be retried and how long to wait first.
func (t *retryTransport) retryAfter(resp *http.Response, attempt int, idempotent, replayable bool) (time.Duration, bool) {
	if !replayable {
		return 0, false
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		wait := t.backoff(attempt)
		// Only integer seconds. The HTTP-date form is legal but the API does
		// not send it, and guessing wrong here means sleeping for hours.
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs >= 0 {
			// A NEGATIVE value is nonsense, and treating it as 0 would fire
			// every remaining retry back-to-back - the opposite of what a 429
			// asks for. It falls through to the backoff, exactly like an
			// absent or unparseable header.
			if time.Duration(secs) > maxRetryAfter/time.Second {
				// Compared in SECONDS, before any multiplication, so a value
				// large enough to overflow a Duration never becomes one.
				wait = maxRetryAfter
			} else {
				wait = time.Duration(secs) * time.Second
			}
		}
		return wait, true
	case resp.StatusCode >= 500 && idempotent:
		return t.backoff(attempt), true
	default:
		return 0, false
	}
}

func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions:
		return true
	default:
		return false
	}
}

// nothingWritten reports whether err proves the request never reached the
// server. A dial or DNS failure happens before any byte of the request is
// written; anything else might have been received and acted on.
func nothingWritten(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// drain empties and closes a response we are about to discard, so the
// connection can be reused. The read is BOUNDED: an error page is
// attacker-influenceable and can be arbitrarily large, and this body is by
// definition one nobody wants - reading it in full to throw it away is a
// denial-of-service surface. Past the limit the connection is simply not
// reused, which costs one handshake.
func drain(resp *http.Response) {
	if resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
	_ = resp.Body.Close()
}

const maxDrainBytes = 64 << 10

// defaultSleep waits for d unless the context ends first, in which case it
// returns the context error and the retry loop gives up.
func defaultSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
