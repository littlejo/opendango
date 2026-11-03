package xo

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
)

// TestConsole runs the client side of the RFB protocol against a scripted
// server on a pipe.
func TestConsole(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	got := make(chan []byte, 1)
	go func() {
		defer server.Close()
		server.Write([]byte("RFB 003.008\n"))
		version := make([]byte, 12)
		io.ReadFull(server, version)
		server.Write([]byte{2, 2, 1}) // VNC auth, none
		choice := make([]byte, 1)
		io.ReadFull(server, choice)
		server.Write([]byte{0, 0, 0, 0})
		shared := make([]byte, 1)
		io.ReadFull(server, shared)
		init := make([]byte, 24)
		binary.BigEndian.PutUint32(init[20:], 4)
		server.Write(append(init, "QEMU"...))
		var keys bytes.Buffer
		io.Copy(&keys, server)
		got <- append(append(version, choice...), append(shared, keys.Bytes()...)...)
	}()
	if err := handshake(client); err != nil {
		t.Fatal(err)
	}
	c := &console{conn: client, done: make(chan struct{})}
	close(c.done)
	c.KeyEvent(0xff0d, true)
	c.KeyEvent(0xff0d, false)
	c.Close()
	want := "RFB 003.008\n\x01\x01" + "\x04\x01\x00\x00\x00\x00\xff\x0d" + "\x04\x00\x00\x00\x00\x00\xff\x0d"
	if g := string(<-got); g != want {
		t.Errorf("client sent %q, want %q", g, want)
	}
}

func TestConsoleRefused(t *testing.T) {
	tests := []struct {
		version, rest string
		err           string
	}{
		{"SSH-2.0-Open", "", "not a VNC server"},
		{"RFB 003.008\n", "\x00\x00\x00\x00\x04nope", "refused: nope"},
		{"RFB 003.008\n", "\x01\x02", "no security type without authentication"},
		{"RFB 003.008\n", "\x01\x01\x00\x00\x00\x01\x00\x00\x00\x03bad", "refused: bad"},
	}
	for _, tt := range tests {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			server.Write([]byte(tt.version))
			// net.Pipe is synchronous: read what the client writes meanwhile.
			go io.Copy(io.Discard, server)
			server.Write([]byte(tt.rest))
		}()
		err := handshake(client)
		if err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("%q: err = %v, want %q", tt.version+tt.rest, err, tt.err)
		}
		client.Close()
	}
}
