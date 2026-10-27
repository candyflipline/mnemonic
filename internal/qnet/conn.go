package qnet

import (
	"net"
	"sync"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/quic-go/quic-go"
)

const serverCloseGrace = 5 * time.Second

type conn struct {
	st *quic.Stream
	qc *quic.Conn

	id     auth.Identity
	served bool

	once sync.Once
}

func (c *conn) Read(b []byte) (int, error)         { return c.st.Read(b) }
func (c *conn) Write(b []byte) (int, error)        { return c.st.Write(b) }
func (c *conn) LocalAddr() net.Addr                { return c.qc.LocalAddr() }
func (c *conn) RemoteAddr() net.Addr               { return c.qc.RemoteAddr() }
func (c *conn) SetDeadline(t time.Time) error      { return c.st.SetDeadline(t) }
func (c *conn) SetReadDeadline(t time.Time) error  { return c.st.SetReadDeadline(t) }
func (c *conn) SetWriteDeadline(t time.Time) error { return c.st.SetWriteDeadline(t) }

// Close ends the stream and releases the QUIC connection. A dialer closes the
// connection at once. The server sends its FIN first and gives the client
// serverCloseGrace to close, so a last response is not cut off.
func (c *conn) Close() error {
	c.once.Do(func() {
		c.st.CancelRead(0)
		c.st.Close()
		if !c.served {
			c.qc.CloseWithError(0, "")
			return
		}
		go func() {
			t := time.NewTimer(serverCloseGrace)
			defer t.Stop()
			select {
			case <-c.qc.Context().Done():
			case <-t.C:
				c.qc.CloseWithError(0, "")
			}
		}()
	})
	return nil
}

func IdentityOf(c net.Conn) (auth.Identity, bool) {
	sc, ok := c.(*conn)
	if !ok || !sc.served {
		return auth.Identity{}, false
	}
	return sc.id, true
}
