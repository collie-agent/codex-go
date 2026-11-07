// Package logging shares the append-only log destination across command diagnostics and events.
package logging

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/collie-agent/codex-go/internal/config"
	"github.com/redis/go-redis/v9"
)

const DefaultDirectory = "logs"

// FileWriter opens per write so callers do not need a file lifecycle or retain descriptors.
type FileWriter struct{ Directory string }

func (w FileWriter) Write(data []byte) (int, error) {
	dir := w.Directory
	if dir == "" {
		dir = DefaultDirectory
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return 0, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "codex.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 0, err
	}
	n, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return n, writeErr
	}
	return n, closeErr
}

// Setup resolves the shared destination; invalid YAML is reported to the explicit or default directory.
func Setup(directory, configPath string) error {
	if configPath != "" {
		cfg, err := config.LoadAgentConfig(configPath)
		if err != nil {
			if setupErr := configure(directory); setupErr != nil {
				return errors.Join(err, setupErr)
			}
			return err
		}
		if directory == "" {
			directory = cfg.LogDir
		}
	}
	return configure(directory)
}

func configure(directory string) error {
	writer := FileWriter{Directory: directory}
	// Fail at startup rather than silently lose the first diagnostic.
	if _, err := writer.Write(nil); err != nil {
		return fmt.Errorf("open log directory: %w", err)
	}
	log.SetOutput(writer)
	log.SetFlags(log.LstdFlags)
	redis.SetLogger(redisLogger{})
	return nil
}

// DiagnosticWriter lets dependencies that write directly to a writer use the same log format.
type DiagnosticWriter struct{ Logger *log.Logger }

func (w DiagnosticWriter) Write(data []byte) (int, error) {
	if err := w.Logger.Output(2, "[INFO] "+string(data)); err != nil {
		return 0, err
	}
	return len(data), nil
}

func NewLogger(writer io.Writer) *log.Logger { return log.New(writer, "", log.LstdFlags) }

// ParseFlags keeps help readable while sending argument errors to the selected log destination.
func ParseFlags() {
	var diagnostics bytes.Buffer
	flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)
	flag.CommandLine.SetOutput(&diagnostics)
	err := flag.CommandLine.Parse(os.Args[1:])
	flag.CommandLine.SetOutput(os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		// Help is a command response, not a diagnostic.
		_, _ = os.Stdout.Write(diagnostics.Bytes())
		os.Exit(0)
	}
	if err == nil {
		return
	}
	directory := ""
	if value := flag.Lookup("log-dir"); value != nil {
		directory = value.Value.String()
	}
	configPath := ""
	if value := flag.Lookup("config"); value != nil {
		configPath = value.Value.String()
	}
	if setupErr := Setup(directory, configPath); setupErr != nil {
		log.Printf("[ERROR] %v", setupErr)
	}
	log.Printf("[ERROR] invalid command arguments: %v", err)
	os.Exit(2)
}

type redisLogger struct{}

func (redisLogger) Printf(_ context.Context, format string, args ...interface{}) {
	log.Printf("[ERROR] redis: "+format, args...)
}
