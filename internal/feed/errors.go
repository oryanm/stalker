package feed

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"syscall"
)

// ErrBodyTooLarge means a response exceeded Options.MaxBodyBytes.
var ErrBodyTooLarge = errors.New("response body too large")

var errTooManyRedirects = fmt.Errorf("stopped after %d redirects", maxRedirects)

// requestError gives a transport failure a short message fit for the UI while
// keeping the original error reachable through errors.Is and errors.As.
type requestError struct {
	msg string
	err error
}

func (e *requestError) Error() string { return e.msg }
func (e *requestError) Unwrap() error { return e.err }

// describe wraps err from a request to host with a human-friendly message.
func describe(err error, host string) error {
	var (
		urlErr     *url.Error
		dnsErr     *net.DNSError
		netErr     net.Error
		certErr    *tls.CertificateVerificationError
		hostErr    x509.HostnameError
		authErr    x509.UnknownAuthorityError
		invalidErr x509.CertificateInvalidError
		privateErr *privateAddressError
	)
	if errors.As(err, &urlErr) {
		if u, perr := url.Parse(urlErr.URL); perr == nil && u.Hostname() != "" {
			host = u.Hostname()
		}
	}

	var msg string
	switch {
	case errors.Is(err, context.Canceled):
		msg = "request cancelled"
	case errors.Is(err, errTooManyRedirects):
		msg = fmt.Sprintf("too many redirects (more than %d)", maxRedirects)
	case errors.As(err, &privateErr):
		msg = fmt.Sprintf("refused to connect to %s: %s is a private network address", host, privateErr.addr)
	case errors.As(err, &dnsErr):
		name := dnsErr.Name
		if name == "" {
			name = host
		}
		switch {
		case dnsErr.IsNotFound:
			msg = "host not found: " + name
		case dnsErr.IsTimeout:
			msg = "DNS lookup timed out: " + name
		default:
			msg = "DNS lookup failed: " + name
		}
	case errors.Is(err, syscall.ECONNREFUSED):
		msg = "connection refused by " + host
	case errors.Is(err, syscall.ECONNRESET):
		msg = "connection reset by " + host
	case errors.As(err, &certErr), errors.As(err, &hostErr), errors.As(err, &authErr), errors.As(err, &invalidErr):
		msg = "invalid TLS certificate for " + host
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		msg = "request timed out"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		msg = "connection closed unexpectedly by " + host
	case urlErr != nil:
		msg = "request failed: " + urlErr.Err.Error()
	default:
		msg = "request failed: " + err.Error()
	}
	return &requestError{msg: msg, err: err}
}
