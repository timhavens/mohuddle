package ui

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// terminalText makes untrusted text inert before application styling is added.
// Preserve ordinary whitespace and Unicode (including emoji joiners), but not
// terminal commands or bidi overrides that can disguise an approval's contents.
func terminalText(value string) string {
	return strings.Map(func(r rune) rune {
		if (unicode.IsControl(r) && r != '\n' && r != '\t') ||
			(r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069') {
			return -1
		}
		return r
	}, ansi.Strip(value))
}

// terminalFrame is a final output boundary for every screen, including errors
// and future UI additions. Only printable text, whitespace, and SGR styling
// belong in a Bubble Tea view: cursor movement and other terminal operations
// are the renderer's responsibility. Raw content is sanitized with terminalText
// before styling; this second layer preserves the application's own colors.
func terminalFrame(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	parser := ansi.NewParser()
	parser.SetHandler(ansi.Handler{
		Print: func(r rune) {
			if !unicode.IsControl(r) {
				out.WriteRune(r)
			}
		},
		Execute: func(b byte) {
			if b == '\n' || b == '\t' {
				out.WriteByte(b)
			}
		},
		HandleCsi: func(cmd ansi.Cmd, params ansi.Params) {
			if cmd != ansi.Cmd('m') {
				return
			}
			out.WriteString("\x1b[")
			separator := byte(';')
			for i, param := range params {
				if i > 0 {
					out.WriteByte(separator)
				}
				out.WriteString(strconv.Itoa(param.Param(0)))
				separator = ';'
				if param.HasMore() {
					separator = ':'
				}
			}
			out.WriteByte('m')
		},
	})
	parser.Parse([]byte(value))
	return out.String()
}
