package guest

import (
	"io"
	"net"
	"testing"

	"github.com/mdlayher/vsock"
)

type cidConn struct {
	net.Conn
	cid uint32
}

func (c *cidConn) RemoteAddr() net.Addr { return &vsock.Addr{ContextID: c.cid, Port: 1234} }

type queuedVSock struct {
	net.Listener
	connections []net.Conn
}

func (l *queuedVSock) Accept() (net.Conn, error) {
	if len(l.connections) == 0 {
		return nil, io.EOF
	}
	c := l.connections[0]
	l.connections = l.connections[1:]
	return c, nil
}

func TestAgentVSockRejectsGuestPeersBeforeHTTP(t *testing.T) {
	q := &queuedVSock{}
	var rejected []net.Conn
	for _, cid := range []uint32{vsock.Local, 3, 42, vsock.Hypervisor} {
		a, b := net.Pipe()
		defer b.Close()
		q.connections = append(q.connections, &cidConn{a, cid})
		rejected = append(rejected, b)
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	host := &cidConn{a, vsock.Host}
	q.connections = append(q.connections, host)
	got, err := (&hostVSockListener{Listener: q}).Accept()
	if err != nil || got != host {
		t.Fatalf("host connection not accepted: %v", err)
	}
	for _, peer := range rejected {
		var buf [1]byte
		if _, err := peer.Read(buf[:]); err != io.EOF {
			t.Fatalf("guest peer was not disconnected: %v", err)
		}
	}
}
