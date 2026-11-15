package setup

import "fmt"

// Refusal is a reason bootstrap will not proceed. The CLI reports it as a
// usage error (exit 2) with Hint as the hint.
type Refusal struct {
	Msg  string
	Hint string
}

func (r *Refusal) Error() string { return r.Msg }

// refuse builds a *Refusal.
func refuse(hint, format string, args ...any) error {
	return &Refusal{Msg: fmt.Sprintf(format, args...), Hint: hint}
}
