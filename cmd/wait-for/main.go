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

	if err := run(flag.Args(), *timeout, *interval); err != nil {
		log.Fatal(err)
	}
}

type mode int

const (
	modeTCP  mode = iota
	modeHTTP mode = iota
)

type target struct {
	mode mode
	addr string // "host:port" for TCP, URL for HTTP
}

func parseTarget(args []string) (target, error) {
	if len(args) == 0 {
		return target{}, fmt.Errorf("usage: wait-for [flags] -- host port\n       wait-for [flags] http://url")
	}

	if len(args) == 1 && (strings.HasPrefix(args[0], "http://") || strings.HasPrefix(args[0], "https://")) {
		return target{mode: modeHTTP, addr: args[0]}, nil
	}

	if len(args) == 2 {
		return target{mode: modeTCP, addr: args[0] + ":" + args[1]}, nil
	}

	return target{}, fmt.Errorf("wait-for: unexpected arguments: %v", args)
}

func run(args []string, timeout, interval time.Duration) error {
	t, err := parseTarget(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	opts := []waiter.Option{waiter.WithInterval(interval)}

	switch t.mode {
	case modeHTTP:
		log.Printf("waiting for HTTP %s", t.addr)
		checker := httpchecker.New(t.addr, httpchecker.WithExpectStatusCode(200))
		if err := waiter.WaitContext(ctx, checker, opts...); err != nil {
			return fmt.Errorf("wait-for: %w", err)
		}
	case modeTCP:
		log.Printf("waiting for TCP %s", t.addr)
		checker := tcpchecker.New(t.addr)
		if err := waiter.WaitContext(ctx, checker, opts...); err != nil {
			return fmt.Errorf("wait-for: %w", err)
		}
	}
	return nil
}
