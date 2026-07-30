// Command lago-mcp is an MCP server exposing Lago billing as agent tools: the
// read surface (billing data) plus the billing-lifecycle write surface (meter
// usage, start/stop a subscription, credit a wallet), each write marked
// destructive so the middleware puts a human in front of it. It always speaks JSON-RPC 2.0 over stdio (Claude Desktop, Claude Code,
// etc.); when an HTTP address is configured it ALSO serves the uniform Gateway
// HTTP Contract v1 (Contract A) so the langgraph brain can list + invoke the
// same tools over HTTP with a Bearer token. Set LAGO_API_URL and LAGO_API_KEY.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gridiron-robotics/lago/integrations/mcp"
)

// defaultHTTPAddr is used when -http is passed without a value / LAGO_MCP_HTTP_ADDR
// is empty. Port 8037 continues the ERP MCP-server range (bigcapital 8035,
// horilla 8036); no billing gateway exists in the langgraph manifest yet.
const defaultHTTPAddr = ":8037"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// -http may be given a value (":9000"); bare "-http" falls back to the env
	// var then the default. Empty (the zero value) means stdio-only.
	httpAddr := flag.String("http", os.Getenv("LAGO_MCP_HTTP_ADDR"),
		"also serve Contract A HTTP on this address (e.g. :8037); empty = stdio only")
	flag.Parse()
	if isSet("http") && *httpAddr == "" {
		*httpAddr = defaultHTTPAddr
	}

	apiURL, apiKey := os.Getenv("LAGO_API_URL"), os.Getenv("LAGO_API_KEY")
	client := mcp.NewLagoClient(apiURL, apiKey, nil)

	// Write tools are registered only when the writer is configured, so an
	// unconfigured deployment advertises no mutations at all rather than tools
	// that fail on every call.
	tools := mcp.ReadOnlyTools(client)
	if writer := mcp.NewLagoWriter(apiURL, apiKey, nil); writer != nil {
		tools = append(tools, mcp.WriteTools(writer)...)
	}
	server := mcp.NewServer("lago", "0.1.0", tools)

	if *httpAddr == "" {
		// stdio-only: unchanged behavior.
		if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "lago-mcp:", err)
			os.Exit(1)
		}
		return
	}

	// HTTP is the long-lived network transport; stdio runs alongside for desktop
	// use and must never terminate the process on EOF (no stdin in a container).
	hs := &http.Server{Addr: *httpAddr, Handler: server.HTTPHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "lago-mcp stdio:", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdownCtx)
	}()
	fmt.Fprintln(os.Stderr, "lago-mcp: Contract A HTTP listening on", *httpAddr)
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "lago-mcp http:", err)
		os.Exit(1)
	}
}

// isSet reports whether the named flag was explicitly provided on the CLI.
func isSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
