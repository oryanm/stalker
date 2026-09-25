package feed

import (
	"errors"
	"fmt"
	"net/netip"
	"syscall"
)

// ErrPrivateAddress means a request was refused because its host resolved to
// a non-public address (see Options.AllowPrivateNetworks).
var ErrPrivateAddress = errors.New("private network address")

type privateAddressError struct{ addr netip.Addr }

func (e *privateAddressError) Error() string {
	return fmt.Sprintf("%v %s refused", ErrPrivateAddress, e.addr)
}
func (e *privateAddressError) Unwrap() error { return ErrPrivateAddress }

// sharedAddressSpace is carrier-grade NAT space, also used by Tailscale and by
// Alibaba Cloud's metadata service.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// refusePrivate is a net.Dialer Control func: it runs after DNS resolution
// for every connection, so redirects and DNS rebinding are covered too.
func refusePrivate(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: unexpected dial address %q", ErrPrivateAddress, address)
	}
	if addr := ap.Addr().Unmap(); !isPublic(addr) {
		return &privateAddressError{addr: addr}
	}
	return nil
}

// isPublic reports whether addr is routable on the public internet as far as
// the checks that matter here go: loopback, private (RFC 1918, fc00::/7),
// link-local (169.254.0.0/16 cloud metadata, fe80::/10), shared, multicast and
// unspecified addresses are not.
func isPublic(addr netip.Addr) bool {
	return addr.IsValid() && !addr.IsLoopback() && !addr.IsPrivate() && !addr.IsLinkLocalUnicast() &&
		!addr.IsLinkLocalMulticast() && !addr.IsInterfaceLocalMulticast() && !addr.IsMulticast() &&
		!addr.IsUnspecified() && !sharedAddressSpace.Contains(addr)
}
