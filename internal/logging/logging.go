// Package logging configures the slog default logger with secret-redacting
// attribute replacement so accidentally logged credentials never reach output.
package logging

import (
	"log/slog"
	"os"
	"regexp"
	"strings"
)

// secretKeyRE matches attribute keys that look like secrets.
var secretKeyRE = regexp.MustCompile(`(?i)secret|token|password|authclient|authsecret|sshkey|api_secret|api_key`)

// New builds a JSON slog.Logger at the given level. Attributes whose key matches
// secretKeyRE are replaced with "<redacted>" regardless of their actual value.
func New(level string) *slog.Logger {
	lvl := parseLevel(level)
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if secretKeyRE.MatchString(a.Key) {
				return slog.Attr{Key: a.Key, Value: slog.StringValue("<redacted>")}
			}
			return a
		},
	})
	return slog.New(h)
}

// parseLevel accepts debug, info, warn, error (case-insensitive). Unknown → info.
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
