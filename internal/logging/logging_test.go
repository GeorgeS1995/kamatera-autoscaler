package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestRedactsSecretAttrs(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if secretKeyRE.MatchString(a.Key) {
				return slog.Attr{Key: a.Key, Value: slog.StringValue("<redacted>")}
			}
			return a
		},
	})
	log := slog.New(h)
	log.Info("hello", "kamatera_secret", "actual-secret-value", "msg", "ok", "auth_token", "the-token-value")
	out := buf.String()
	if strings.Contains(out, "actual-secret-value") || strings.Contains(out, "the-token-value") {
		t.Errorf("secret leaked: %s", out)
	}
	if !strings.Contains(out, "<redacted>") {
		t.Errorf("expected <redacted> in output: %s", out)
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug, "info": slog.LevelInfo,
		"warn": slog.LevelWarn, "warning": slog.LevelWarn,
		"error": slog.LevelError, "err": slog.LevelError,
		"": slog.LevelInfo, "wat": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}
