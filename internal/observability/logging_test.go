package observability

import (
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{
			input: "debug",
			want:  slog.LevelDebug,
		},
		{
			input: "info",
			want:  slog.LevelInfo,
		},
		{
			input: "warn",
			want:  slog.LevelWarn,
		},
		{
			input: "error",
			want:  slog.LevelError,
		},
		{
			input: "unknown",
			want:  slog.LevelInfo,
		},
	}

	for _, test := range tests {
		t.Run(
			test.input,
			func(t *testing.T) {
				got := parseLevel(test.input)

				if got != test.want {
					t.Fatalf(
						"parseLevel(%q) = %v, want %v",
						test.input,
						got,
						test.want,
					)
				}
			},
		)
	}
}
