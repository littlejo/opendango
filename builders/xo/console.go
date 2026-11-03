package xo

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/coder/websocket"
	"github.com/hashicorp/packer-plugin-sdk/bootcommand"
)

// SendKeys types keys, in the boot_command syntax of Packer, into the VM
// console. The keys are sent every 100ms (PACKER_KEY_INTERVAL changes it).
func (b *Builder) SendKeys(ctx context.Context, keys string) error {
	seq, err := bootcommand.GenerateExpressionSequence(keys)
	if err != nil {
		return err
	}
	c, err := b.openConsole(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	return seq.Do(ctx, bootcommand.NewVNCDriver(c, 0))
}

// console is the VNC console of a VM, through XO's console proxy
// (/api/consoles/{id}, the WebSocket the XO web UI uses). It only sends key
// events: the RFB handshake, then KeyEvent messages (RFC 6143).
type console struct {
	conn net.Conn
	done chan struct{}
}

// openConsole connects to the console of the VM. XO checks the token
// (cookie "token") and the operate permission on the VM.
func (b *Builder) openConsole(ctx context.Context) (*console, error) {
	wsURL := "ws" + strings.TrimPrefix(b.client.BaseURL, "http") + "/api/consoles/" + b.vmID
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPClient:   b.client.HTTP,
		HTTPHeader:   http.Header{"Cookie": {"token=" + b.client.Token}},
		Subprotocols: []string{"binary"},
	})
	if err != nil {
		return nil, fmt.Errorf("opening the console: %w", err)
	}
	// The connection lives until Close, not until ctx is done.
	conn := websocket.NetConn(context.WithoutCancel(ctx), ws, websocket.MessageBinary)
	if err := handshake(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("opening the console: %w", err)
	}
	c := &console{conn: conn, done: make(chan struct{})}
	// Nothing is requested, but the server may still send (bell, clipboard).
	go func() {
		defer close(c.done)
		io.Copy(io.Discard, conn)
	}()
	return c, nil
}

// handshake runs the RFB 3.8 handshake without authentication (XO already
// checked the token) and asks for a shared session.
func handshake(rw io.ReadWriter) error {
	version := make([]byte, 12)
	if _, err := io.ReadFull(rw, version); err != nil {
		return err
	}
	if !strings.HasPrefix(string(version), "RFB 003.") {
		return fmt.Errorf("not a VNC server: %q", version)
	}
	if _, err := rw.Write([]byte("RFB 003.008\n")); err != nil {
		return err
	}
	n := make([]byte, 1)
	if _, err := io.ReadFull(rw, n); err != nil {
		return err
	}
	if n[0] == 0 {
		return fmt.Errorf("refused: %s", reason(rw))
	}
	types := make([]byte, n[0])
	if _, err := io.ReadFull(rw, types); err != nil {
		return err
	}
	if !strings.ContainsRune(string(types), 1) {
		return fmt.Errorf("no security type without authentication (offered: %v)", types)
	}
	if _, err := rw.Write([]byte{1}); err != nil {
		return err
	}
	result := make([]byte, 4)
	if _, err := io.ReadFull(rw, result); err != nil {
		return err
	}
	if binary.BigEndian.Uint32(result) != 0 {
		return fmt.Errorf("refused: %s", reason(rw))
	}
	// ClientInit (shared), then ServerInit: size, pixel format, name.
	if _, err := rw.Write([]byte{1}); err != nil {
		return err
	}
	init := make([]byte, 24)
	if _, err := io.ReadFull(rw, init); err != nil {
		return err
	}
	_, err := io.CopyN(io.Discard, rw, int64(binary.BigEndian.Uint32(init[20:])))
	return err
}

// reason reads the reason string of a failure.
func reason(r io.Reader) string {
	n := make([]byte, 4)
	if _, err := io.ReadFull(r, n); err != nil {
		return "no reason given"
	}
	msg := make([]byte, min(binary.BigEndian.Uint32(n), 1024))
	io.ReadFull(r, msg)
	return string(msg)
}

// KeyEvent presses (down) or releases a key, given as an X11 keysym.
func (c *console) KeyEvent(key uint32, down bool) error {
	msg := []byte{4, 0, 0, 0, 0, 0, 0, 0}
	if down {
		msg[1] = 1
	}
	binary.BigEndian.PutUint32(msg[4:], key)
	_, err := c.conn.Write(msg)
	return err
}

// Close closes the console.
func (c *console) Close() error {
	err := c.conn.Close()
	<-c.done
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
