package pool

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// warnf writes a warning-class runtime line at slog Warn, so a console log
// level of warn keeps it (std log lines go through slog at Info). The line
// is redacted (router.Redact): no URLs, no full 32-byte ids.
func warnf(format string, a ...any) {
	if slog.Default().Enabled(context.Background(), slog.LevelWarn) {
		slog.Warn(router.Redact(fmt.Sprintf(format, a...)))
	}
}
