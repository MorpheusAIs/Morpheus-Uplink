package api

import (
	"context"
	"fmt"
	"log/slog"
)

// warnf writes a warning-class runtime line at slog Warn, so a console log
// level of warn keeps it (std log lines go through slog at Info).
func warnf(format string, a ...any) {
	if slog.Default().Enabled(context.Background(), slog.LevelWarn) {
		slog.Warn(fmt.Sprintf(format, a...))
	}
}
