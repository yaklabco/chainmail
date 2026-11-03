package cli

import (
	"fmt"
	"io"

	"github.com/charmbracelet/fang"
)

// fangOptions configures fang for chainmail. chainmail owns its completion
// and man commands, so they behave the same with and without a terminal,
// and keeps its version command rather than a --version flag.
func fangOptions() []fang.Option {
	return []fang.Option{
		fang.WithoutCompletions(),
		fang.WithoutManpage(),
		fang.WithoutVersion(),
		fang.WithErrorHandler(styledError),
	}
}

// styledError shows err to a person: the reason as the header, then the
// message and the hint. classify decides all three, as it does for JSON
// errors, so both modes report the same failure.
func styledError(w io.Writer, styles fang.Styles, err error) {
	c := classify(err)
	_, _ = fmt.Fprintln(w, styles.ErrorHeader.SetString(c.reason).String())
	_, _ = fmt.Fprintln(w, styles.ErrorText.Render(termText(c.message)))
	if c.hint != "" {
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, styles.ErrorText.Render(termText(c.hint)))
	}
	_, _ = fmt.Fprintln(w)
}
