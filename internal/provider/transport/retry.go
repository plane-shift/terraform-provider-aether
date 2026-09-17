// Package transport provides the HTTP RoundTripper the provider's API client
// uses for every request.
package transport

import "net/http"

// bearerTransport adds the Authorization: Bearer <token> header to every
// request. Task 3 replaces this body with retry/backoff behavior; the
// signature stays the same.
type bearerTransport struct {
	token string
	next  http.RoundTripper
}

// New returns an http.RoundTripper that adds the bearer token header before
// delegating to next.
func New(token string, next http.RoundTripper) http.RoundTripper {
	return &bearerTransport{token: token, next: next}
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	return t.next.RoundTrip(req)
}
