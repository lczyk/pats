package proxy

import (
	"bufio"
	"net"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// a tcp connection to the relay reaches whatever listens on the socket, and
// both directions get through.
func TestRelay(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "p.sock")
	up, err := net.Listen("unix", sock)
	require.NoError(t, err)
	defer up.Close()
	go func() {
		c, err := up.Accept()
		if err != nil {
			return
		}
		line, _ := bufio.NewReader(c).ReadString('\n')
		_, _ = c.Write([]byte("echo " + line))
		c.Close()
	}()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go Relay(ln, sock)
	c, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer c.Close()
	_, err = c.Write([]byte("hi\n"))
	require.NoError(t, err)
	got, _ := bufio.NewReader(c).ReadString('\n')
	assert.Equal(t, got, "echo hi\n")
}
