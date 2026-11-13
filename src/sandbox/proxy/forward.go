package proxy

import (
	"io"
	"net"
	"sync"
)

// Relay accepts on ln until it closes and joins each connection to a fresh
// one to the unix socket at socket: the bwrap driver's forwarder, which
// gives a sandbox with no network a tcp proxy address that reaches the
// proxy on the host through a bound socket.
func Relay(ln net.Listener, socket string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			up, err := net.Dial("unix", socket)
			if err != nil {
				c.Close()
				return
			}
			var wg sync.WaitGroup
			wg.Add(2)
			join := func(dst, src net.Conn) {
				defer wg.Done()
				_, _ = io.Copy(dst, src)
				if cw, ok := dst.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				} else {
					dst.Close()
				}
			}
			go join(up, c)
			go join(c, up)
			wg.Wait()
			c.Close()
			up.Close()
		}()
	}
}
