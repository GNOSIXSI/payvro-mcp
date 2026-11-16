package main

import (
	"context"
	"fmt"
	"os"

	redactor "github.com/GNOSIXSI/payvro-mcp/internal/redact"
	"github.com/GNOSIXSI/payvro-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	secrets := redactor.ActiveEnvSecrets()
	stdout, stderr, logger := installStreams(secrets...)
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		fmt.Fprintf(stderr, "payvro-mcp: panic: %v\n", rec)
		flush(stdout)
		flush(stderr)
		os.Exit(2)
	}()
	maybeTestPanic()
	server := mcp.NewServer(&mcp.Implementation{Name: "payvro", Version: "0.1.0"}, &mcp.ServerOptions{
		Logger: logger,
	})
	tools.Register(server, stderr)
	err := server.Run(context.Background(), &mcp.IOTransport{Reader: os.Stdin, Writer: stdout})
	_ = stdout.Close()
	if err != nil {
		fmt.Fprintf(stderr, "payvro-mcp: %s\n", redact(err.Error()))
		flush(stderr)
		os.Exit(1)
	}
	flush(stderr)
}

func redact(msg string) string {
	return redactor.RedactString(msg, redactor.ActiveEnvSecrets()...)
}
