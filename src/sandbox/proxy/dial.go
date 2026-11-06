package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// Guard decides which addresses the proxy may dial at all, whatever the
// policy says. it runs on the address actually being connected to, after
// resolution, so a name that resolves (or re-resolves) to a private address
// is caught there and not at some earlier lookup.
type Guard struct {
	// AllowPrivate lifts the refusal of loopback, link-local, private (rfc1918
	// and ula) and v4-mapped targets. unspecified addresses and Refuse stay
	// refused regardless.
	AllowPrivate bool
	// Refuse lists ranges never dialled, such as the subnet pool pats runs its
	// sandboxes in.
	Refuse []netip.Prefix
}

// RefusedError is a dial the Guard stopped.
type RefusedError struct {
	Addr   netip.Addr
	Reason string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("dial %s refused: %s", e.Addr, e.Reason)
}

// check is the verdict for one target address.
func (g *Guard) check(addr netip.Addr) error {
	addr = addr.Unmap()
	if addr.IsUnspecified() {
		return &RefusedError{addr, "unspecified"}
	}
	for _, pfx := range g.Refuse {
		if pfx.Contains(addr) {
			return &RefusedError{addr, "refused-range"}
		}
	}
	if g.AllowPrivate {
		return nil
	}
	switch {
	case addr.IsLoopback(), addr.IsPrivate(), addr.IsLinkLocalUnicast(),
		addr.IsLinkLocalMulticast(), addr.IsInterfaceLocalMulticast(), addr.IsMulticast():
		return &RefusedError{addr, "private"}
	}
	return nil
}

func (g *Guard) control(_ context.Context, _, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	return g.check(ap.Addr())
}

// Dialer is the one dialer every upstream connection goes through. a nil
// guard refuses private targets and nothing else.
func Dialer(g *Guard) *net.Dialer {
	if g == nil {
		g = &Guard{}
	}
	return &net.Dialer{Timeout: 30 * time.Second, ControlContext: g.control}
}

// dialReason names a failed dial for the audit.
func dialReason(err error) string {
	var r *RefusedError
	if errors.As(err, &r) {
		return r.Reason
	}
	return "dial"
}
