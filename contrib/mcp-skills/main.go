// gascity-mcp-skills is an opt-in catalog adapter, never started by gc itself.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/mcpskills"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type repeated []string

func (r *repeated) String() string         { return strings.Join(*r, ",") }
func (r *repeated) Set(value string) error { *r = append(*r, value); return nil }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var catalogs, origins repeated
	flag.Var(&catalogs, "catalog", "Explicit namespace=/absolute/skill-root (repeatable)")
	flag.Var(&origins, "origin", "Allowed HTTP Origin (repeatable; denied by default)")
	listen := flag.String("listen", "", "Optional loopback HTTP address, e.g. 127.0.0.1:8765; default stdio")
	pageSize := flag.Int("page-size", 100, "Skills per page (1–100)")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	roots := make(map[string]string)
	for _, entry := range catalogs {
		namespace, dir, ok := strings.Cut(entry, "=")
		if !ok || dir == "" {
			return fmt.Errorf("catalog must be namespace=directory")
		}
		if _, exists := roots[namespace]; exists {
			return fmt.Errorf("duplicate catalog namespace %q", namespace)
		}
		roots[namespace] = dir
	}
	catalog, err := mcpskills.Load(roots)
	if err != nil {
		return err
	}
	server, err := mcpskills.NewServer(catalog, *pageSize)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *listen == "" {
		return server.SDK.Run(ctx, &mcp.StdioTransport{})
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("HTTP must bind to a loopback IP; use an authenticated TLS proxy for remote access")
	}
	token := os.Getenv("GC_SKILLS_MCP_TOKEN")
	if len(token) < 32 {
		return fmt.Errorf("GC_SKILLS_MCP_TOKEN must contain at least 32 bytes")
	}
	handler, err := server.HTTPHandler(token, origins)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	httpServer := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := httpServer.Shutdown(shutdownCtx); err != nil {
				fmt.Fprintln(os.Stderr, "shutdown:", err)
			}
		case <-done:
		}
	}()
	err = httpServer.ListenAndServe()
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
