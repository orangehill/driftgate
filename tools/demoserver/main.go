// Command demoserver runs the DriftGate demo API locally:
//
//	go run ./tools/demoserver -addr 127.0.0.1:8099 -version v1
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/orangehill/driftgate/internal/demo"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8099", "listen address")
	version := flag.String("version", "v1", "v1 (baseline) or v2 (with planted drift)")
	flag.Parse()
	log.Printf("demo API %s on http://%s", *version, *addr)
	log.Fatal(http.ListenAndServe(*addr, demo.New(*version)))
}
