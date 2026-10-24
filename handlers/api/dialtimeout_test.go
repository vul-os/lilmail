package api

import (
	"net"
	"testing"
	"time"
)

// A server that accepts the TCP connection but never sends the IMAP greeting
// must not hang dialIMAP: the dial timeout doubles as the greeting deadline.
func TestDialIMAP_MuteServerTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold it open, say nothing
		}
	}()

	old := imapDialer
	imapDialer = &net.Dialer{Timeout: 300 * time.Millisecond}
	defer func() { imapDialer = old }()

	addr := ln.Addr().(*net.TCPAddr)
	start := time.Now()
	_, err = dialIMAP("127.0.0.1", addr.Port, false)
	if err == nil {
		t.Fatal("expected an error from a mute server")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("dial took %v; the timeout did not apply", d)
	}
}
