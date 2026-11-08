// Command latencyproxy forwards TCP connections from a listen address to a
// target and delays every chunk by a fixed time in each direction, so a
// round trip through it costs twice the delay, as over a slow link. It
// makes measurements of round trips repeatable on one machine.
//
// To measure the MySQL fingerprint over a 54 ms round trip, put the proxy
// in front of a test server and point DRIFTGATE_TEST_MYSQL at it:
//
//	go run ./tools/latencyproxy -listen 127.0.0.1:33299 -target 127.0.0.1:3306 -delay 27ms &
//	DRIFTGATE_TEST_MYSQL='mysql://root:secret@127.0.0.1:33299/' \
//		go test ./internal/state -run '^$' -bench 'BenchmarkMySQL(Fingerprint|Probe)' -benchtime 5x
//
// Compare with the same run against the server directly to see what the
// round trips cost.
package main

import (
	"errors"
	"flag"
	"log"
	"net"
	"sync"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:33299", "address to listen on")
	target := flag.String("target", "", "address to forward to (host:port)")
	delay := flag.Duration("delay", 0, "one-way delay added in each direction, such as 27ms")
	flag.Parse()
	if *target == "" || *delay < 0 {
		flag.Usage()
		log.Fatal("latencyproxy: -target is required and -delay may not be negative")
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("latencyproxy: %s -> %s, %s each way", ln.Addr(), *target, *delay)
	log.Fatal(serve(ln, *target, *delay))
}

// serve accepts connections on ln until it is closed and forwards each to
// target with the delay.
func serve(ln net.Listener, target string, delay time.Duration) error {
	for {
		client, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go func() {
			server, err := net.Dial("tcp", target)
			if err != nil {
				log.Printf("latencyproxy: %v", err)
				client.Close()
				return
			}
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); forward(server, client, delay) }()
			go func() { defer wg.Done(); forward(client, server, delay) }()
			wg.Wait()
			client.Close()
			server.Close()
		}()
	}
}

type chunk struct {
	data []byte
	due  time.Time
}

// forward copies src to dst, each chunk delay after it was read. Chunks
// read meanwhile are not held up by the wait of the one before (the link
// is slow, not narrow). At the end of src the write side of dst is closed;
// when dst fails, src is closed so the other direction ends too.
func forward(dst, src net.Conn, delay time.Duration) {
	queue := make(chan chunk, 1024)
	go func() {
		defer close(queue)
		for {
			buf := make([]byte, 32<<10)
			n, err := src.Read(buf)
			if n > 0 {
				queue <- chunk{buf[:n], time.Now().Add(delay)}
			}
			if err != nil {
				return
			}
		}
	}()
	for c := range queue {
		time.Sleep(time.Until(c.due))
		if _, err := dst.Write(c.data); err != nil {
			src.Close()
			for range queue {
			}
			return
		}
	}
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	} else {
		dst.Close()
	}
}
