package daemon

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// cacheConfig has one account with the cache and one with it turned off.
const cacheConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"

[accounts.off]
provider = "hostinger"
address = "off@example.com"

[accounts.off.cache]
enabled = false
`

// cacheOffConfig turns the cache off for every account.
const cacheOffConfig = passwordConfig + `
[cache]
enabled = false
`

// openCache opens cache.db in CacheDir, creating the directory with mode
// 0700 and replacing a file that is not a cache, and leaves the daemon
// without a cache, never failing, when it is off or cannot open.
func TestOpenCache(t *testing.T) {
	t.Parallel()
	newDir := func(t *testing.T) string { return filepath.Join(t.TempDir(), "cache") }
	tests := []struct {
		name     string
		config   string
		disable  bool
		dir      func(t *testing.T) string // "" means no CacheDir
		wantOpen bool
		wantErr  bool
	}{
		{name: "opens and creates the directory", config: cacheConfig, dir: newDir, wantOpen: true},
		{name: "no cache directory", config: cacheConfig, dir: func(*testing.T) string { return "" }},
		{name: "off by option", config: cacheConfig, disable: true, dir: newDir},
		{name: "off for every account", config: cacheOffConfig, dir: newDir},
		{
			name: "the directory is a file", config: cacheConfig, wantErr: true,
			dir: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(p, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
		{
			name: "a file that is not a cache is replaced", config: cacheConfig, wantOpen: true,
			dir: func(t *testing.T) string {
				// openCache leaves an existing directory's mode alone, as
				// runDaemon does the state directory's, so this one is
				// made 0700 here: t.TempDir's subdirectories are 0777
				// less the umask.
				d := filepath.Join(t.TempDir(), "cache")
				if err := os.Mkdir(d, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(d, cacheFile), []byte("not a database, just some text"), 0o600); err != nil {
					t.Fatal(err)
				}
				return d
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := tt.dir(t)
			s, _ := newTestServerOptions(t, tt.config, Options{CacheDir: dir, DisableCache: tt.disable})
			s.openCache(t.Context())
			t.Cleanup(s.closeCache)
			if got := s.cache != nil; got != tt.wantOpen {
				t.Fatalf("cache open = %v, want %v (open error %v)", got, tt.wantOpen, s.cacheErr)
			}
			if got := s.cacheErr != nil; got != tt.wantErr {
				t.Errorf("open error = %v, want an error: %v", s.cacheErr, tt.wantErr)
			}
			_, statErr := os.Stat(filepath.Join(dir, cacheFile))
			if !tt.wantOpen {
				if dir != "" && !tt.wantErr && !errors.Is(statErr, fs.ErrNotExist) {
					t.Errorf("cache.db exists without a cache: %v", statErr)
				}
				return
			}
			if statErr != nil {
				t.Errorf("cache.db: %v", statErr)
			}
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != 0o700 {
				t.Errorf("cache dir mode = %o, want 700", perm)
			}
			if _, err := s.cache.SizeBytes(t.Context()); err != nil {
				t.Errorf("SizeBytes: %v", err)
			}
		})
	}
}
