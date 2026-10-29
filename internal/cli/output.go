package cli

import (
	"encoding/json"
	"fmt"
	"io"
)

// writeJSON writes v to w as one line of JSON.
func writeJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding output: %w", err)
	}
	b = append(b, '\n')
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}
