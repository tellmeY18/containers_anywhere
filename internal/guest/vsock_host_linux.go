package guest

import (
	"net"

	"github.com/mdlayher/vsock"
)

// Host CID is supplied by the virtio transport, not by an HTTP header. Reject
// guest-local and other guest CIDs before any private agent route is served.
// TCP/QEMU uses per-boot mutual TLS instead.
type hostVSockListener struct{ net.Listener }

func (l *hostVSockListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		addr, ok := c.RemoteAddr().(*vsock.Addr)
		if ok && addr.ContextID == vsock.Host {
			return c, nil
		}
		_ = c.Close()
	}
}
