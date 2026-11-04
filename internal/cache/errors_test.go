package cache

import (
	"errors"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

func TestCodeErr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		code int
		want error
	}{
		{"full", sqlite3.SQLITE_FULL, ErrDiskFull},
		{"corrupt", sqlite3.SQLITE_CORRUPT, ErrCorrupt},
		{"corrupt vtab, extended", sqlite3.SQLITE_CORRUPT_VTAB, ErrCorrupt},
		{"not a database", sqlite3.SQLITE_NOTADB, ErrCorrupt},
		{"constraint", sqlite3.SQLITE_CONSTRAINT, nil},
		{"not null constraint, extended", sqlite3.SQLITE_CONSTRAINT_NOTNULL, nil},
		{"busy", sqlite3.SQLITE_BUSY, nil},
		{"io error nomem, extended", sqlite3.SQLITE_IOERR_NOMEM, nil},
		{"ok", sqlite3.SQLITE_OK, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := codeErr(tt.code)
			if tt.want == nil && got != nil || tt.want != nil && !errors.Is(got, tt.want) {
				t.Errorf("codeErr(%d) = %v, want %v", tt.code, got, tt.want)
			}
		})
	}
}

func TestClassifyLeavesOtherErrors(t *testing.T) {
	t.Parallel()
	got := classify(errors.New("plain"))
	if got.Error() != "plain" || errors.Is(got, ErrDiskFull) || errors.Is(got, ErrCorrupt) {
		t.Errorf("classify(plain) = %v, want it unchanged", got)
	}
}
