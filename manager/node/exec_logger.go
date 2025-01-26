package node

import (
	"bufio"
	"bytes"
	"log/slog"
)

type execLogger struct {
	logger *slog.Logger
	level  slog.Level
}

func newExecLogger(name string, l *slog.Logger, level slog.Level) *execLogger {
	return &execLogger{
		logger: l.With("exec", name),
		level:  level,
	}
}

func (el execLogger) Write(p []byte) (n int, err error) {
	scanner := bufio.NewScanner(bytes.NewReader(p))
	for scanner.Scan() {
		el.logger.Debug(scanner.Text())
	}
	return len(p), nil
}
