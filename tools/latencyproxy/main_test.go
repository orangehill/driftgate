package main

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

// TestRoundTrip: a round trip through the proxy to an echo server takes at
// least twice the delay, and the bytes arrive as sent.
func TestRoundTrip(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	const delay = 30 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- serve(ln, echo.Addr().String(), delay) }()
	defer func() {
		ln.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	payload := bytes.Repeat([]byte("driftgate "), 10000) // several chunks
	for i := 0; i < 3; i++ {
		start := time.Now()
		go c.Write(payload)
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(c, got); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(start); took < 2*delay {
			t.Fatalf("round trip took %s, want at least %s", took, 2*delay)
		} else {
			t.Logf("round trip %d: %s", i, took)
		}
		if !bytes.Equal(got, payload) {
			t.Fatal("the bytes changed on the way")
		}
	}
}
