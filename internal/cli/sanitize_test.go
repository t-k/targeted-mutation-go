package cli

import (
	"testing"
	"unicode/utf8"
)

func TestSanitizeInline(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain ASCII unchanged",
			input: "x >= 10 && flag",
			want:  "x >= 10 && flag",
		},
		{
			name:  "CSI colors and cursor movement stripped",
			input: "\x1b[31mred\x1b[0m \x1b[2J\x1b[1;1Htext\x1b[?25l",
			want:  "red text",
		},
		{
			name:  "CSI with intermediate bytes stripped",
			input: "before\x1b[1 qafter",
			want:  "beforeafter",
		},
		{
			name:  "unterminated CSI stripped",
			input: "prefix\x1b[31;1",
			want:  "prefix",
		},
		{
			name:  "OSC 8 hyperlink BEL terminated stripped",
			input: "\x1b]8;;https://evil.example\x07click\x1b]8;;\x07",
			want:  "click",
		},
		{
			name:  "OSC 8 hyperlink ST terminated stripped",
			input: "\x1b]8;;https://evil.example\x1b\\click\x1b]8;;\x1b\\",
			want:  "click",
		},
		{
			name:  "OSC title with C1 ST stripped",
			input: "\x1b]0;forged title\u009cvisible",
			want:  "visible",
		},
		{
			name:  "unterminated OSC stripped to end",
			input: "safe\x1b]0;forged title",
			want:  "safe",
		},
		{
			name:  "DCS SOS PM APC sequences stripped",
			input: "a\x1bP1$r\x1b\\b\x1bXsos\x1b\\c\x1b^pm\x07d\x1b_apc\u009ce",
			want:  "abcde",
		},
		{
			name:  "nF and single-byte escape sequences stripped",
			input: "\x1b(Bascii\x1b7\x1b8\x1bc!",
			want:  "ascii!",
		},
		{
			name:  "C1 CSI and C1 string controls stripped",
			input: "\u009b31mwarn\u009b0m \u009d0;title\x07ok\u0090dcs\x1b\\\u0098sos\u009c\u009epm\x07\u009fapc\u009c!",
			want:  "warn ok!",
		},
		{
			name:  "Trojan Source BiDi overrides and isolates stripped",
			input: "isAdmin\u202e \u2066/*\u2069\u061c\u200e\u200f\u202a\u202b\u202c\u202d\u2067\u2068",
			want:  "isAdmin /*",
		},
		{
			name:  "forged newline and carriage return overwrite flattened",
			input: "ok\nINPUT GAP fake.go:1:1\r\n  expression: forged\rOVERWRITE\tcol",
			want:  "ok INPUT GAP fake.go:1:1   expression: forged OVERWRITE col",
		},
		{
			name:  "NUL BEL BS DEL and C1 controls stripped",
			input: "a\x00b\x07c\x08d\x7fe\u0080f\u0085g\u009f",
			want:  "abcdefg",
		},
		{
			name:  "valid unicode emoji Japanese and accented text preserved",
			input: "値 := \"🎉 café 日本語\"",
			want:  "値 := \"🎉 café 日本語\"",
		},
		{
			name:  "invalid UTF-8 bytes replaced with U+FFFD",
			input: "bad\xff\xfeutf8\xc3",
			want:  "bad" + string(utf8.RuneError) + string(utf8.RuneError) + "utf8" + string(utf8.RuneError),
		},
		{
			name:  "raw 8-bit C1 CSI OSC and single-byte controls stripped",
			input: "head\x9b31mRED\x9b0m\x9d0;spoofed-title\x9c\x85tail",
			want:  "headREDtail",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeInline(tc.input); got != tc.want {
				t.Errorf("SanitizeInline(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestSanitizeBlock(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "preserves newlines and tabs while normalizing CRLF and lone CR",
			input: "line1\r\n\tindented\nline3\roverwrite",
			want:  "line1\n\tindented\nline3 overwrite",
		},
		{
			name:  "strips ANSI CSI OSC C1 BiDi NUL DEL and C0 controls",
			input: "\x1b[31merror:\x1b[0m \x1b]8;;https://evil.example\x07link\x1b]8;;\x1b\\\n\t\u202edetail\u009b1m!\u009b0m\x00\x07\x08\x7f\u0085",
			want:  "error: link\n\tdetail!",
		},
		{
			name:  "unterminated OSC and DCS sequences stripped",
			input: "line1\n\x1b]0;unterminated",
			want:  "line1\n",
		},
		{
			name:  "valid unicode and invalid UTF-8 handled cleanly",
			input: "詳細: 🎉 café\n\xff",
			want:  "詳細: 🎉 café\n" + string(utf8.RuneError),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeBlock(tc.input); got != tc.want {
				t.Errorf("SanitizeBlock(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
