package system

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func me() (uid, gid uint32) {
	return uint32(os.Getuid()), uint32(os.Getgid())
}

func wantMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %v, want %v", path, got, want)
	}
}

func TestOSFiles(t *testing.T) {
	t.Parallel()
	uid, gid := me()
	var f OSFiles

	t.Run("EnsureDir creates", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "state")
		if err := f.EnsureDir(dir, uid, gid, 0o750); err != nil {
			t.Fatalf("EnsureDir: %v", err)
		}
		wantMode(t, dir, 0o750)
	})

	t.Run("EnsureDir fixes the mode", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := f.EnsureDir(dir, uid, gid, 0o700); err != nil {
			t.Fatalf("EnsureDir: %v", err)
		}
		wantMode(t, dir, 0o700)
	})

	t.Run("EnsureDir refuses a file", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "state")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := f.EnsureDir(path, uid, gid, 0o700); err == nil {
			t.Fatal("EnsureDir accepted a regular file")
		}
	})

	t.Run("EnsureDir refuses a symlink", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		target := filepath.Join(root, "real")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "state")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := f.EnsureDir(link, uid, gid, 0o700); err == nil {
			t.Fatal("EnsureDir accepted a symlink")
		}
		wantMode(t, target, 0o755)
	})

	t.Run("WriteFile creates and replaces", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		if err := f.WriteFile(path, []byte("one"), uid, gid, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		wantMode(t, path, 0o644)
		if err := f.WriteFile(path, []byte("two"), uid, gid, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		wantMode(t, path, 0o600)
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "two" {
			t.Fatalf("contents = %q, %v", got, err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("directory holds %d entries, want only config.toml", len(entries))
		}
	})

	t.Run("WriteFile replaces a symlink without following it", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		target := filepath.Join(dir, "elsewhere")
		if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "config.toml")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if err := f.WriteFile(path, []byte("new"), uid, gid, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if got, _ := os.ReadFile(target); string(got) != "keep" {
			t.Fatalf("symlink target changed to %q", got)
		}
		fi, err := os.Lstat(path)
		if err != nil || fi.Mode()&fs.ModeSymlink != 0 {
			t.Fatalf("path is still a symlink: %v", err)
		}
	})

	t.Run("WriteFile into a missing directory", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "missing", "config.toml")
		if err := f.WriteFile(path, []byte("x"), uid, gid, 0o644); err == nil {
			t.Fatal("WriteFile succeeded without its directory")
		}
	})

	t.Run("CreateExclusive", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "vault-key")
		if err := f.CreateExclusive(path, []byte("secret"), uid, gid, 0o600); err != nil {
			t.Fatalf("CreateExclusive: %v", err)
		}
		wantMode(t, path, 0o600)
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "secret" {
			t.Fatalf("contents = %q, %v", got, err)
		}
		err = f.CreateExclusive(path, []byte("other"), uid, gid, 0o600)
		if !errors.Is(err, fs.ErrExist) {
			t.Fatalf("second CreateExclusive = %v, want fs.ErrExist", err)
		}
		if got, _ := os.ReadFile(path); string(got) != "secret" {
			t.Fatalf("existing file changed to %q", got)
		}
	})

	t.Run("CreateExclusive refuses a symlink", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		path := filepath.Join(dir, "vault-key")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if err := f.CreateExclusive(path, []byte("secret"), uid, gid, 0o600); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("CreateExclusive = %v, want fs.ErrExist", err)
		}
		if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("symlink target was created: %v", err)
		}
	})

	t.Run("Lstat", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		file := filepath.Join(dir, "file")
		if err := os.WriteFile(file, []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(file, 0o640); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link")
		if err := os.Symlink("../somewhere", link); err != nil {
			t.Fatal(err)
		}

		fi, err := f.Lstat(file)
		if err != nil {
			t.Fatalf("Lstat: %v", err)
		}
		if !fi.Exists || fi.Mode != 0o640 || fi.UID != uid || fi.GID != gid || fi.LinkDest != "" {
			t.Fatalf("file info = %+v", fi)
		}

		fi, err = f.Lstat(dir)
		if err != nil || !fi.Exists || !fi.Mode.IsDir() {
			t.Fatalf("dir info = %+v, %v", fi, err)
		}

		fi, err = f.Lstat(link)
		if err != nil {
			t.Fatalf("Lstat: %v", err)
		}
		if !fi.Exists || fi.Mode&fs.ModeSymlink == 0 || fi.LinkDest != "../somewhere" {
			t.Fatalf("link info = %+v", fi)
		}

		fi, err = f.Lstat(filepath.Join(dir, "missing"))
		if err != nil || fi.Exists {
			t.Fatalf("missing info = %+v, %v", fi, err)
		}
	})

	t.Run("ReadFile", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := f.ReadFile(path)
		if err != nil || string(got) != "hello" {
			t.Fatalf("ReadFile = %q, %v", got, err)
		}
		link := filepath.Join(dir, "link")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		if _, err := f.ReadFile(link); err == nil {
			t.Fatal("ReadFile followed a symlink")
		}
		if _, err := f.ReadFile(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("ReadFile of a missing file = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("RemoveFile", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, ".probe")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := f.RemoveFile(path); err != nil {
			t.Fatalf("RemoveFile: %v", err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("after RemoveFile, Lstat = %v, want fs.ErrNotExist", err)
		}
		// A symlink is removed, never its target.
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte("kept"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := f.RemoveFile(link); err != nil {
			t.Fatalf("RemoveFile of a link: %v", err)
		}
		if got, err := os.ReadFile(target); err != nil || string(got) != "kept" {
			t.Fatalf("the link's target = %q, %v, want it kept", got, err)
		}
		if err := f.RemoveFile(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("RemoveFile of a missing file = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("Lock", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), ".bootstrap.lock")
		unlock, err := f.Lock(path)
		if err != nil {
			t.Fatalf("Lock: %v", err)
		}
		wantMode(t, path, 0o600)

		got := make(chan error, 1)
		go func() {
			unlock2, err := f.Lock(path)
			if err == nil {
				err = unlock2()
			}
			got <- err
		}()

		select {
		case err := <-got:
			t.Fatalf("second Lock returned before unlock: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
		if err := unlock(); err != nil {
			t.Fatalf("unlock: %v", err)
		}
		select {
		case err := <-got:
			if err != nil {
				t.Fatalf("second Lock: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("second Lock still blocked 2 seconds after unlock")
		}
	})
}

func TestRunAsCmd(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	in := strings.NewReader("input")
	cmd := runAsCmd(t.Context(), 995, 994, in, &out, &errOut, "chainmail", "admin", "status")

	wantEnv := []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/var/empty", "LANG=C"}
	if !reflect.DeepEqual(cmd.Env, wantEnv) {
		t.Fatalf("Env = %q, want %q", cmd.Env, wantEnv)
	}
	if want := []string{"chainmail", "admin", "status"}; !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("Args = %q, want %q", cmd.Args, want)
	}
	if cmd.Dir != "/" {
		t.Fatalf("Dir = %q, want /", cmd.Dir)
	}
	if cmd.Stdin != in || cmd.Stdout != &out || cmd.Stderr != &errOut {
		t.Fatal("stdio not passed through")
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatal("no credential set")
	}
	c := cmd.SysProcAttr.Credential
	if c.Uid != 995 || c.Gid != 994 || !reflect.DeepEqual(c.Groups, []uint32{994}) || c.NoSetGroups {
		t.Fatalf("credential = %+v, want uid 995, gid 994, groups [994], NoSetGroups false", *c)
	}
}

func TestRunAs(t *testing.T) {
	t.Parallel()
	if os.Getuid() != 0 {
		t.Skip("RunAs changes credentials, which needs root")
	}
	const nobody = 65534
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"-u"}, "65534\n"},
		{[]string{"-g"}, "65534\n"},
		{[]string{"-G"}, "65534\n"},
	}
	for _, tt := range tests {
		var out, errOut bytes.Buffer
		if err := RunAs(t.Context(), nobody, nobody, nil, &out, &errOut, "id", tt.args...); err != nil {
			t.Fatalf("RunAs id %v: %v (%s)", tt.args, err, errOut.String())
		}
		if out.String() != tt.want {
			t.Fatalf("id %v = %q, want %q", tt.args, out.String(), tt.want)
		}
	}

	var out, errOut bytes.Buffer
	if err := RunAs(t.Context(), nobody, nobody, nil, &out, &errOut, "env"); err != nil {
		t.Fatalf("RunAs env: %v (%s)", err, errOut.String())
	}
	if want := "PATH=/usr/sbin:/usr/bin:/sbin:/bin\nHOME=/var/empty\nLANG=C\n"; out.String() != want {
		t.Fatalf("env = %q, want %q", out.String(), want)
	}
}

func TestRunAsMissingCommand(t *testing.T) {
	t.Parallel()
	uid, gid := me()
	err := RunAs(t.Context(), uid, gid, nil, nil, nil, filepath.Join(t.TempDir(), "no-such-command"))
	if err == nil {
		t.Fatal("RunAs succeeded for a missing command")
	}
}
