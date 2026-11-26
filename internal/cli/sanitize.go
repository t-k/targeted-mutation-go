package cli

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// SanitizeInline strips terminal escape sequences, directional formatting,
// and control characters from single-line report fields, replacing line breaks
// and tabs with spaces so embedded newlines cannot forge report lines.
func SanitizeInline(s string) string {
	return sanitizeTerminal(s, false)
}

// SanitizeBlock strips terminal escape sequences, directional formatting, and
// unsafe control characters from multi-line diagnostic text while preserving
// newlines and tabs.
func SanitizeBlock(s string) string {
	return sanitizeTerminal(s, true)
}

func sanitizeTerminal(s string, multiline bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\x1b' {
			i = skipEscapeSequence(s, i)
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			raw := s[i]
			if raw >= 0x80 && raw <= 0x9F {
				if next, handled := skipC1Sequence(s, i, rune(raw), 1); handled {
					i = next
				} else {
					i++
				}
				continue
			}
			b.WriteRune(utf8.RuneError)
			i++
			continue
		}
		if next, handled := skipC1Sequence(s, i, r, size); handled {
			i = next
			continue
		}
		switch r {
		case '\r':
			if i+1 < len(s) && s[i+1] == '\n' {
				i += 2
				writeBreak(&b, multiline)
				continue
			}
			b.WriteByte(' ')
			i++
			continue
		case '\n':
			writeBreak(&b, multiline)
			i++
			continue
		case '\t':
			if multiline {
				b.WriteByte('\t')
			} else {
				b.WriteByte(' ')
			}
			i++
			continue
		}
		if isBidiControl(r) || unicode.IsControl(r) {
			i += size
			continue
		}
		b.WriteRune(r)
		i += size
	}
	return b.String()
}

func writeBreak(b *strings.Builder, multiline bool) {
	if multiline {
		b.WriteByte('\n')
		return
	}
	b.WriteByte(' ')
}

func skipEscapeSequence(s string, i int) int {
	if i+1 >= len(s) {
		return i + 1
	}
	switch s[i+1] {
	case '[':
		return skipCSI(s, i+2)
	case ']', 'P', 'X', '^', '_':
		return skipControlString(s, i+2)
	default:
		j := i + 1
		for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2F {
			j++
		}
		if j < len(s) && s[j] >= 0x30 && s[j] <= 0x7E {
			return j + 1
		}
		return j
	}
}

func skipC1Sequence(s string, i int, r rune, size int) (int, bool) {
	switch r {
	case 0x9B:
		return skipCSI(s, i+size), true
	case 0x90, 0x98, 0x9D, 0x9E, 0x9F:
		return skipControlString(s, i+size), true
	default:
		return 0, false
	}
}

func skipCSI(s string, i int) int {
	for i < len(s) && s[i] >= 0x30 && s[i] <= 0x3F {
		i++
	}
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2F {
		i++
	}
	if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7E {
		return i + 1
	}
	return i
}

func skipControlString(s string, i int) int {
	for i < len(s) {
		if s[i] == '\x07' || s[i] == 0x9C {
			return i + 1
		}
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '\\' {
			return i + 2
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == '\u009c' {
			return i + size
		}
		i += size
	}
	return i
}

func isBidiControl(r rune) bool {
	return r == 0x061C || r == 0x200E || r == 0x200F ||
		(r >= 0x202A && r <= 0x202E) ||
		(r >= 0x2066 && r <= 0x2069)
}
