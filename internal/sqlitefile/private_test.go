package sqlitefile

import (
	"bytes"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// permissiveDir makes a 0755 directory, whatever the umask, as a
// configured cache_dir that already exists may be.
func permissiveDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeMode writes data to path and sets its mode, whatever the umask.
func writeMode(t *testing.T, path string, data []byte, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// wantMode fails the test when path exists with a mode other than want.
// With mustExist it also fails when path is missing.
func wantMode(t *testing.T, path string, want fs.FileMode, mustExist bool) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		if mustExist || !os.IsNotExist(err) {
			t.Errorf("stat %s: %v", filepath.Base(path), err)
		}
		return
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", filepath.Base(path), got, want)
	}
}

// openWrite opens path, writes a row so the -wal and -shm files exist, and
// closes the connection at test end.
func openWrite(t *testing.T, path string) *sql.DB {
	t.Helper()
	var reasons []string
	conn := openTest(t, path, &reasons)
	if _, err := conn.ExecContext(t.Context(), "INSERT INTO kept VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestOpenCreatesPrivateDatabaseInPermissiveDirectory(t *testing.T) {
	t.Parallel()
	dir := permissiveDir(t)
	path := filepath.Join(dir, "test.db")
	openWrite(t, path)
	wantMode(t, path, 0o600, true)
	wantMode(t, path+"-wal", 0o600, true)
	wantMode(t, path+"-shm", 0o600, true)
	// The directory's own mode is the operator's choice.
	wantMode(t, dir, 0o755, true)
}

func TestOpenTightensExistingFiles(t *testing.T) {
	t.Parallel()
	path := filepath.Join(permissiveDir(t), "test.db")
	if err := openWrite(t, path).Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	// Empty sidecars left by an earlier run: SQLite reads an empty -wal as
	// no log and rebuilds an empty -shm.
	writeMode(t, path+"-wal", nil, 0o644)
	writeMode(t, path+"-shm", nil, 0o664)
	openWrite(t, path)
	wantMode(t, path, 0o600, true)
	wantMode(t, path+"-wal", 0o600, true)
	wantMode(t, path+"-shm", 0o600, true)
}

func TestOpenRecreatesPrivate(t *testing.T) {
	t.Parallel()
	path := filepath.Join(permissiveDir(t), "test.db")
	writeMode(t, path, bytes.Repeat([]byte("not a database "), 300), 0o644)
	openWrite(t, path)
	wantMode(t, path, 0o600, true)
	wantMode(t, path+"-wal", 0o600, true)
	wantMode(t, path+"-shm", 0o600, true)
}

func TestMakePrivateTightensMarker(t *testing.T) {
	t.Parallel()
	path := filepath.Join(permissiveDir(t), "cache.db")
	writeMode(t, MarkerPath(path), nil, 0o644)
	writeMode(t, path, nil, 0o640)
	if err := MakePrivate(path); err != nil {
		t.Fatalf("MakePrivate: %v", err)
	}
	wantMode(t, MarkerPath(path), 0o600, true)
	wantMode(t, path, 0o600, true)
}

func TestMakePrivateCreatesAbsentDatabase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(permissiveDir(t), "cache.db")
	if err := MakePrivate(path); err != nil {
		t.Fatalf("MakePrivate: %v", err)
	}
	wantMode(t, path, 0o600, true)
	// Nothing else is created: SQLite makes its sidecars itself.
	for _, p := range []string{path + "-wal", path + "-shm", MarkerPath(path)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists after MakePrivate: %v", filepath.Base(p), err)
		}
	}
}

func TestMakePrivateMissingDirectory(t *testing.T) {
	t.Parallel()
	if err := MakePrivate(filepath.Join(t.TempDir(), "absent", "cache.db")); err == nil {
		t.Fatal("MakePrivate in a missing directory succeeded, want an error")
	}
}
