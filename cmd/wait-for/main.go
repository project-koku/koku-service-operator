// wait-for blocks until TCP endpoints or HTTP URLs become reachable.
// It is compiled into the operator image so init containers can reuse the
// operator image rather than pulling a separate wait-for image.
//
// Usage:
//
//	/wait-for [--timeout 10m] [--interval 2s] -- host port   (TCP)
//	/wait-for [--timeout 10m] [--interval 5s] http://url     (HTTP 200 only)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	httpchecker "wait4x.dev/v3/checker/http"
	tcpchecker "wait4x.dev/v3/checker/tcp"
	"wait4x.dev/v3/waiter"
)

func main() {
	timeout := flag.Duration("timeout", 10*time.Minute, "total wait deadline")
	interval := flag.Duration("interval", 2*time.Second, "poll interval")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: wait-for [flags] -- host port")
		fmt.Fprintln(os.Stderr, "       wait-for [flags] http://url")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	opts := []waiter.Option{waiter.WithInterval(*interval)}

	// HTTP URL: single argument starting with http:// or https://
	if len(args) == 1 && (strings.HasPrefix(args[0], "http://") || strings.HasPrefix(args[0], "https://")) {
		log.Printf("waiting for HTTP %s", args[0])
		checker := httpchecker.New(args[0], httpchecker.WithExpectStatusCode(200))
		if err := waiter.WaitContext(ctx, checker, opts...); err != nil {
			log.Fatalf("wait-for: %v", err)
		}
		return
	}

	// TCP: two arguments — host and port (no shell joining)
	if len(args) == 2 {
		addr := args[0] + ":" + args[1]
		log.Printf("waiting for TCP %s", addr)
		checker := tcpchecker.New(addr)
		if err := waiter.WaitContext(ctx, checker, opts...); err != nil {
			log.Fatalf("wait-for: %v", err)
		}
		return
	}

	fmt.Fprintf(os.Stderr, "wait-for: unexpected arguments: %v\n", args)
	os.Exit(1)
}
