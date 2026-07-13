package logging

import (
	"context"
	"log/slog"
	"testing"
)

func TestLoggerRespectsConfiguredLevel(t *testing.T) {
	t.Setenv("LOG_LEVEL", "warn")
	logger := New("test")
	if logger.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("info should be disabled at warn level")
	}
	if !logger.Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("warn should be enabled")
	}
}
