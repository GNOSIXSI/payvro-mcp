package main

import (
	"io"
	"log"
	"log/slog"
	"os"

	redactor "github.com/GNOSIXSI/payvro-mcp/internal/redact"
)

// installStreams wraps the process stdout and stderr in synchronous redacting
// writers. The SDK stdio transport receives stdout. log, slog and the SDK
// logger write diagnostics to stderr. File descriptors stay put, so the same
// path works on linux, darwin and windows. Handler redaction stays in front.
func installStreams(secrets ...string) (stdout io.WriteCloser, stderr io.Writer, logger *slog.Logger) {
	errW := redactor.NewWriter(os.Stderr, secrets...)
	log.SetOutput(errW)
	logger = slog.New(slog.NewTextHandler(errW, &slog.HandlerOptions{Level: slog.LevelError}))
	slog.SetDefault(logger)
	return flushWriter{redactor.NewWriter(os.Stdout, secrets...)}, errW, logger
}

func flush(w io.Writer) {
	if f, ok := w.(interface{ Flush() error }); ok {
		_ = f.Flush()
	}
}

type flushWriter struct{ *redactor.Writer }

func (f flushWriter) Close() error { return f.Flush() }
