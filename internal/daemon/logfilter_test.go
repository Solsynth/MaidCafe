package daemon

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestShouldUploadLog(t *testing.T) {
	for _, tc := range []struct {
		name    string
		line    string
		minimum string
		want    bool
	}{
		{name: "no filter", line: `{"level":"debug"}`, minimum: "", want: true},
		{name: "json warn above info", line: `{"level":"warn","msg":"slow"}`, minimum: "info", want: true},
		{name: "json warn below error", line: `{"level":"warn"}`, minimum: "error", want: false},
		{name: "json uppercase error", line: `{"level":"ERROR"}`, minimum: "error", want: true},
		{name: "json severity alias", line: `{"severity":"warning"}`, minimum: "warn", want: true},
		{name: "json numeric error", line: `{"level":50}`, minimum: "error", want: true},
		{name: "json numeric info below warn", line: `{"level":30}`, minimum: "warn", want: false},
		{name: "nested logger level", line: `{"log":{"level":"debug"}}`, minimum: "info", want: false},
		{name: "plain warning", line: "2026-11-19T21:57:28Z WARNING disk almost full", minimum: "warn", want: true},
		{name: "plain info below warn", line: "2026-11-19T21:57:28Z INFO started", minimum: "warn", want: false},
		{name: "unparseable is uploaded", line: "raw output without a level", minimum: "fatal", want: true},
		{name: "empty level uploads", line: `{"level":""}`, minimum: "error", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldUploadLog(tc.line, tc.minimum); got != tc.want {
				t.Fatalf("shouldUploadLog(%q, %q) = %v, want %v", tc.line, tc.minimum, got, tc.want)
			}
		})
	}
}

func TestSanitizeUploadLogLine(t *testing.T) {
	long := strings.Repeat("x", 5000)
	got := sanitizeUploadLogLine(long)
	if len(got) > maxUploadLogLineBytes || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Fatalf("sanitized line length = %d, valid = %v, suffix = %q", len(got), utf8.ValidString(got), got[len(got)-3:])
	}

	// Cutting inside a multi-byte rune must back off to a valid boundary.
	multibyte := strings.Repeat("日", 2000)
	got = sanitizeUploadLogLine(multibyte)
	if len(got) > maxUploadLogLineBytes || !utf8.ValidString(got) {
		t.Fatalf("multibyte line length = %d, valid = %v", len(got), utf8.ValidString(got))
	}

	if got := sanitizeUploadLogLine("line\r\n"); got != "line\n" {
		t.Fatalf("CRLF sanitization = %q", got)
	}
}
