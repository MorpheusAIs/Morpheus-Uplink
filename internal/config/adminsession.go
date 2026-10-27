package config

import (
	"os"
	"strings"
	"time"
)

// Admin session lengths (ADMIN_SESSION_IDLE / ADMIN_SESSION_ABSOLUTE).
// Env only: they are not settable from the GUI, so an admin session cannot
// lengthen itself. They do not depend on the 2FA state.
const (
	AdminSessionIdleDefault     = 4 * time.Hour
	AdminSessionAbsoluteDefault = 12 * time.Hour
	AdminSessionAbsoluteMin     = 4 * time.Hour  // documented lower bound (shorter is kept, with a warning)
	AdminSessionAbsoluteMax     = 12 * time.Hour // hard upper bound (longer is clamped, with a warning)

	// Fallbacks for a value that is not a positive Go duration.
	AdminSessionIdleFallback     = 30 * time.Minute
	AdminSessionAbsoluteFallback = 8 * time.Hour
)

// ParseAdminSession turns the raw ADMIN_SESSION_IDLE and
// ADMIN_SESSION_ABSOLUTE values into effective durations. It never fails:
// a value is only ever moved in the shorter direction, and every change
// from what was set is reported in warnings (names and effective values
// only). Rules:
//   - absolute: unset → 12h; not a positive duration → 8h; above 12h → 12h;
//     below 4h → kept, with an "under the documented range" warning.
//   - idle: unset → 4h; not a positive duration → 30m; an explicit value
//     below 4h is kept.
//   - idle above absolute → idle = absolute.
func ParseAdminSession(idleRaw, absRaw string) (idle, absolute time.Duration, warnings []string) {
	idleRaw, absRaw = strings.TrimSpace(idleRaw), strings.TrimSpace(absRaw)

	absolute = AdminSessionAbsoluteDefault
	if absRaw != "" {
		d, err := time.ParseDuration(absRaw)
		switch {
		case err != nil || d <= 0:
			absolute = AdminSessionAbsoluteFallback
			warnings = append(warnings, "ADMIN_SESSION_ABSOLUTE is not a positive duration (for example 12h); using "+AdminSessionAbsoluteFallback.String())
		case d > AdminSessionAbsoluteMax:
			absolute = AdminSessionAbsoluteMax
			warnings = append(warnings, "ADMIN_SESSION_ABSOLUTE is above "+AdminSessionAbsoluteMax.String()+"; using "+AdminSessionAbsoluteMax.String())
		case d < AdminSessionAbsoluteMin:
			absolute = d
			warnings = append(warnings, "ADMIN_SESSION_ABSOLUTE "+d.String()+" is under the documented range ("+AdminSessionAbsoluteMin.String()+" to "+AdminSessionAbsoluteMax.String()+"); keeping it")
		default:
			absolute = d
		}
	}

	idle = AdminSessionIdleDefault
	if idleRaw != "" {
		d, err := time.ParseDuration(idleRaw)
		if err != nil || d <= 0 {
			idle = AdminSessionIdleFallback
			warnings = append(warnings, "ADMIN_SESSION_IDLE is not a positive duration (for example 4h); using "+AdminSessionIdleFallback.String())
		} else {
			idle = d
		}
	}
	if idle > absolute {
		if idleRaw != "" {
			warnings = append(warnings, "ADMIN_SESSION_IDLE "+idle.String()+" is above ADMIN_SESSION_ABSOLUTE; using "+absolute.String())
		}
		idle = absolute
	}
	return idle, absolute, warnings
}

func (c *Config) loadAdminSession() {
	c.AdminSessionIdle, c.AdminSessionAbsolute, c.AdminSessionWarnings =
		ParseAdminSession(os.Getenv("ADMIN_SESSION_IDLE"), os.Getenv("ADMIN_SESSION_ABSOLUTE"))
}
