//go:build e2e

// Command e2e-relay listens on one address and copies every connection both
// ways to another. The end-to-end suite runs it in the appliance, where pco
// takes the Cloudflare API of tests on a loopback address only, to reach the
// fake the suite serves on the node.
package main

import (
	"flag"
	"io"
	"log"
	"net"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8787", "the address to listen on")
	to := flag.String("to", "", "the address to forward to")
	flag.Parse()
	if *to == "" {
		log.Fatal("-to is required")
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	for {
		in, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go relay(in, *to)
	}
}

// relay copies a connection to the target and back. The client's end of
// input is passed on; the target's end of output ends the connection.
func relay(in net.Conn, to string) {
	defer func() { _ = in.Close() }()
	out, err := net.DialTimeout("tcp", to, 10*time.Second)
	if err != nil {
		log.Printf("dialling %s: %v", to, err)
		return
	}
	defer func() { _ = out.Close() }()
	go func() {
		_, _ = io.Copy(out, in)
		if tcp, ok := out.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	_, _ = io.Copy(in, out)
}
