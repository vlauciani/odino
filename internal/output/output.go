// Package output renders command results as ASCII tables or JSON.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/mattn/go-isatty"
)

// Format selects the render mode.
type Format int

const (
	FormatTable Format = iota // aligned ASCII table (with color on TTY)
	FormatJSON                // pretty JSON
)

// Table renders a header + rows as an aligned ASCII table to w. Rows may have ragged
// length; missing cells render as empty strings.
func Table(w io.Writer, header []string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(header) > 0 {
		fmt.Fprintln(tw, strings.Join(header, "\t"))
		sep := make([]string, len(header))
		for i, h := range header {
			sep[i] = strings.Repeat("-", maxLen(2, len(h)))
		}
		fmt.Fprintln(tw, strings.Join(sep, "\t"))
	}
	for _, row := range rows {
		// pad/truncate to header length
		if len(header) > 0 && len(row) < len(header) {
			padded := make([]string, len(header))
			copy(padded, row)
			row = padded
		}
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	return tw.Flush()
}

// JSON pretty-prints v to w.
func JSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// IsTTY reports whether w is an interactive terminal (used to decide on color/banner styling).
func IsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// Banner prints a one-line ⚠ banner to w. Uses ANSI yellow on TTY.
func Banner(w io.Writer, msg string) {
	if IsTTY(w) {
		fmt.Fprintf(w, "\x1b[33m⚠ %s\x1b[0m\n", msg)
		return
	}
	fmt.Fprintf(w, "! %s\n", msg)
}

func maxLen(a, b int) int {
	if a > b {
		return a
	}
	return b
}
