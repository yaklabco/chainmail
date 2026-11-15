package e2e

import (
	"encoding/pem"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/yaklabco/chainmail/internal/setup"
)

func TestMutateHostBlocked(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		env     map[string]string
		allowed bool
	}{
		{name: "both set", env: map[string]string{"CI": "true", "CHAINMAIL_E2E_MUTATE_HOST": "1"}, allowed: true},
		{name: "nothing set", env: map[string]string{}},
		{name: "only CI", env: map[string]string{"CI": "true"}},
		{name: "only the opt-in", env: map[string]string{"CHAINMAIL_E2E_MUTATE_HOST": "1"}},
		{name: "CI is 1, not true", env: map[string]string{"CI": "1", "CHAINMAIL_E2E_MUTATE_HOST": "1"}},
		{name: "opt-in is true, not 1", env: map[string]string{"CI": "true", "CHAINMAIL_E2E_MUTATE_HOST": "true"}},
		{name: "opt-in is empty", env: map[string]string{"CI": "true", "CHAINMAIL_E2E_MUTATE_HOST": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reason := mutateHostBlocked(func(k string) string { return tc.env[k] })
			if tc.allowed && reason != "" {
				t.Errorf("blocked with %q, want allowed", reason)
			}
			if !tc.allowed && reason == "" {
				t.Error("allowed, want blocked")
			}
		})
	}
}

func TestHostIDs(t *testing.T) {
	t.Parallel()
	ids := hostIDs{DaemonUID: 301, DaemonGID: 302, ClientsGID: 303}
	owners := map[setup.Owner]uint32{setup.OwnerRoot: 0, setup.OwnerDaemon: 301}
	for o, want := range owners {
		got, err := ids.owner(o)
		if err != nil || got != want {
			t.Errorf("owner(%q) = %d, %v; want %d", o, got, err, want)
		}
	}
	groups := map[setup.Group]uint32{setup.GroupRoot: 0, setup.GroupDaemon: 302, setup.GroupClients: 303}
	for g, want := range groups {
		got, err := ids.group(g)
		if err != nil || got != want {
			t.Errorf("group(%q) = %d, %v; want %d", g, got, err, want)
		}
	}
	if _, err := ids.owner("nobody"); err == nil {
		t.Error("owner(nobody) did not fail")
	}
	if _, err := ids.group("nobody"); err == nil {
		t.Error("group(nobody) did not fail")
	}
}

func TestCheckEntry(t *testing.T) {
	t.Parallel()
	ids := hostIDs{DaemonUID: 301, DaemonGID: 302, ClientsGID: 303}
	key := setup.Entry{
		Name: "vault-key", Path: "/etc/chainmail/vault-key", Kind: setup.KindFile,
		Owner: setup.OwnerDaemon, Group: setup.GroupDaemon, Mode: 0o400,
	}
	dir := setup.Entry{
		Name: "config-dir", Path: "/etc/chainmail", Kind: setup.KindDir,
		Owner: setup.OwnerRoot, Group: setup.GroupRoot, Mode: 0o755,
	}
	cases := []struct {
		name  string
		entry setup.Entry
		state pathState
		want  []string // substrings of the error; none means no error
	}{
		{name: "file as planned", entry: key, state: pathState{Mode: 0o400, UID: 301, GID: 302}},
		{name: "dir as planned", entry: dir, state: pathState{Mode: fs.ModeDir | 0o755}},
		{name: "wrong mode", entry: key, state: pathState{Mode: 0o440, UID: 301, GID: 302}, want: []string{"mode -r--r-----, want -r--------"}},
		{name: "wrong owner", entry: key, state: pathState{Mode: 0o400, UID: 0, GID: 302}, want: []string{"owner 0, want 301"}},
		{name: "wrong group", entry: key, state: pathState{Mode: 0o400, UID: 301, GID: 303}, want: []string{"group 303, want 302"}},
		{
			name: "every fault at once", entry: key, state: pathState{Mode: 0o644, UID: 1000, GID: 1000},
			want: []string{"mode", "owner 1000", "group 1000"},
		},
		{name: "symlink", entry: key, state: pathState{Mode: fs.ModeSymlink | 0o777, UID: 301, GID: 302}, want: []string{"is a symlink"}},
		{name: "file where a dir belongs", entry: dir, state: pathState{Mode: 0o755}, want: []string{"is not a directory"}},
		{name: "dir where a file belongs", entry: key, state: pathState{Mode: fs.ModeDir | 0o400, UID: 301, GID: 302}, want: []string{"is not a regular file"}},
		{name: "setuid file", entry: key, state: pathState{Mode: fs.ModeSetuid | 0o400, UID: 301, GID: 302}, want: []string{"mode ur--------, want -r--------"}},
		{name: "setgid file", entry: key, state: pathState{Mode: fs.ModeSetgid | 0o400, UID: 301, GID: 302}, want: []string{"mode gr--------, want -r--------"}},
		{name: "setgid dir", entry: dir, state: pathState{Mode: fs.ModeDir | fs.ModeSetgid | 0o755}, want: []string{"mode"}},
		{name: "sticky dir", entry: dir, state: pathState{Mode: fs.ModeDir | fs.ModeSticky | 0o755}, want: []string{"mode"}},
		{name: "every special bit", entry: key, state: pathState{Mode: fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky | 0o400, UID: 301, GID: 302}, want: []string{"mode"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkEntry(tc.entry, tc.state, ids)
			if len(tc.want) == 0 {
				if err != nil {
					t.Errorf("checkEntry = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkEntry = nil, want an error with %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.entry.Path) {
				t.Errorf("error %q does not name %s", err, tc.entry.Path)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

func TestDecodeResult(t *testing.T) {
	t.Parallel()
	got, err := decodeResult([]byte(`{"account":"e2e","provider":"hostinger","paths":{"config":"/etc/chainmail/config.toml"},` +
		`"service_running":true,"test":"ok","key_shown":false,"next_steps":["chainmail accounts list"]}` + "\n"))
	if err != nil {
		t.Fatalf("decodeResult: %v", err)
	}
	if got.Account != "e2e" || got.Test != "ok" || !got.ServiceRunning || got.Paths["config"] != "/etc/chainmail/config.toml" {
		t.Errorf("decodeResult = %+v", got)
	}
	for _, bad := range []string{"", "not json", `{"test":"ok"} {"test":"ok"}`, `[]`} {
		if _, err := decodeResult([]byte(bad)); err == nil {
			t.Errorf("decodeResult(%q) did not fail", bad)
		}
	}
}

func TestStartedAt(t *testing.T) {
	t.Parallel()
	got, err := startedAt([]byte(`{"version":"dev","started_at":"2026-11-15T10:00:00.123456Z","dev_mode":false,"secret_names":[]}`))
	if err != nil || got != "2026-11-15T10:00:00.123456Z" {
		t.Errorf("startedAt = %q, %v", got, err)
	}
	for _, bad := range []string{"", "{}", `{"started_at":""}`, `{"started_at":null}`, "nope"} {
		if _, err := startedAt([]byte(bad)); err == nil {
			t.Errorf("startedAt(%q) did not fail", bad)
		}
	}
}

func TestLowestFreeID(t *testing.T) {
	t.Parallel()
	listing := "_amavisd                 83\nroot                     0\nnobody                   -2\n" +
		"runner                   501\n_chainmail               600\nother                    601\n\n"
	cases := []struct {
		lo, hi int
		want   int
		fails  bool
	}{
		{lo: 600, hi: 700, want: 602},
		{lo: 500, hi: 600, want: 500},
		{lo: 501, hi: 502, fails: true},
		{lo: 83, hi: 84, fails: true},
	}
	for _, tc := range cases {
		got, err := lowestFreeID(listing, tc.lo, tc.hi)
		if tc.fails {
			if err == nil {
				t.Errorf("lowestFreeID(%d, %d) = %d, want an error", tc.lo, tc.hi, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("lowestFreeID(%d, %d) = %d, %v; want %d", tc.lo, tc.hi, got, err, tc.want)
		}
	}
	if _, err := lowestFreeID("name notanumber\n", 1, 10); err == nil {
		t.Error("lowestFreeID accepted a listing line without a number")
	}
}

func TestShellCommand(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		`'/tmp/x/chainmail' 'accounts' 'list'`:              {"/tmp/x/chainmail", "accounts", "list"},
		`'/Library/Application Support/chainmail' 'status'`: {"/Library/Application Support/chainmail", "status"},
		`'it'\''s' ''`: {"it's", ""},
	}
	for want, args := range cases {
		if got := shellCommand(args...); got != want {
			t.Errorf("shellCommand(%q) = %s, want %s", args, got, want)
		}
	}
}

func TestCLIConfigPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		env  map[string]string
		want string
	}{
		{env: map[string]string{"XDG_CONFIG_HOME": "/x", "HOME": "/home/r"}, want: "/x/chainmail/cli.toml"},
		{env: map[string]string{"XDG_CONFIG_HOME": "rel", "HOME": "/home/r"}, want: "/home/r/.config/chainmail/cli.toml"},
		{env: map[string]string{"HOME": "/Users/runner"}, want: "/Users/runner/.config/chainmail/cli.toml"},
		{env: map[string]string{}, want: ""},
	}
	for _, tc := range cases {
		if got := cliConfigPath(func(k string) string { return tc.env[k] }); got != tc.want {
			t.Errorf("cliConfigPath(%v) = %q, want %q", tc.env, got, tc.want)
		}
	}
}

func TestInstallChanges(t *testing.T) {
	t.Parallel()
	base := func() installState {
		return installState{
			StartedAt: "2026-11-15T10:00:00Z",
			Paths: map[string]pathState{
				"/etc/chainmail":           {Mode: fs.ModeDir | 0o755},
				"/etc/chainmail/vault-key": {Mode: 0o400, UID: 301, GID: 302},
			},
			Sums: map[string]string{"/etc/chainmail/vault-key": "aaaa"},
		}
	}
	cases := []struct {
		name   string
		change func(*installState)
		want   []string // substrings, one per reported change, in order
	}{
		{name: "unchanged", change: func(*installState) {}},
		{name: "restarted", change: func(s *installState) { s.StartedAt = "2026-11-15T10:05:00Z" }, want: []string{"started_at 2026-11-15T10:00:00Z, then 2026-11-15T10:05:00Z"}},
		{
			name: "mode",
			change: func(s *installState) {
				s.Paths["/etc/chainmail/vault-key"] = pathState{Mode: 0o440, UID: 301, GID: 302}
			},
			want: []string{"/etc/chainmail/vault-key: mode -r--------, then -r--r-----"},
		},
		{
			name:   "setgid bit",
			change: func(s *installState) { s.Paths["/etc/chainmail"] = pathState{Mode: fs.ModeDir | fs.ModeSetgid | 0o755} },
			want:   []string{"/etc/chainmail: mode drwxr-xr-x, then dgrwxr-xr-x"},
		},
		{
			name:   "owner",
			change: func(s *installState) { s.Paths["/etc/chainmail/vault-key"] = pathState{Mode: 0o400, UID: 0, GID: 302} },
			want:   []string{"/etc/chainmail/vault-key: owner 301, then 0"},
		},
		{
			name:   "group",
			change: func(s *installState) { s.Paths["/etc/chainmail/vault-key"] = pathState{Mode: 0o400, UID: 301, GID: 0} },
			want:   []string{"/etc/chainmail/vault-key: group 302, then 0"},
		},
		{
			name:   "contents",
			change: func(s *installState) { s.Sums["/etc/chainmail/vault-key"] = "bbbb" },
			want:   []string{"/etc/chainmail/vault-key: SHA-256 aaaa, then bbbb"},
		},
		{
			name:   "gone",
			change: func(s *installState) { delete(s.Paths, "/etc/chainmail"); delete(s.Sums, "/etc/chainmail/vault-key") },
			want:   []string{"/etc/chainmail: gone", "/etc/chainmail/vault-key: SHA-256 aaaa, then none"},
		},
		{
			name:   "new",
			change: func(s *installState) { s.Paths["/etc/chainmail/other"] = pathState{Mode: 0o644} },
			want:   []string{"/etc/chainmail/other: new"},
		},
		{
			name: "several, in path order",
			change: func(s *installState) {
				s.Paths["/etc/chainmail/vault-key"] = pathState{Mode: 0o600, UID: 0, GID: 0}
				s.Paths["/etc/chainmail"] = pathState{Mode: fs.ModeDir | 0o775}
			},
			want: []string{"/etc/chainmail: mode", "/etc/chainmail/vault-key: mode", "/etc/chainmail/vault-key: owner", "/etc/chainmail/vault-key: group"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			after := base()
			tc.change(&after)
			got := installChanges(base(), after)
			if len(got) != len(tc.want) {
				t.Fatalf("installChanges = %q, want %d changes like %q", got, len(tc.want), tc.want)
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("change %d = %q, want it to hold %q", i, got[i], w)
				}
			}
		})
	}
}

func TestBundleHolds(t *testing.T) {
	t.Parallel()
	pemBlock := func(der string) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte(der)}))
	}
	bundle := []byte(pemBlock("one") + "\n" + pemBlock("two"))
	if !bundleHolds(bundle, []byte("two")) {
		t.Error("bundleHolds missed the second certificate")
	}
	if bundleHolds(bundle, []byte("three")) {
		t.Error("bundleHolds found a certificate the bundle lacks")
	}
	if bundleHolds(nil, []byte("one")) {
		t.Error("bundleHolds found a certificate in an empty bundle")
	}
	if bundleHolds([]byte(pemBlock("twofold")), []byte("two")) {
		t.Error("bundleHolds matched part of a certificate")
	}
}

func TestKeychainHasSHA1(t *testing.T) {
	t.Parallel()
	listing := "SHA-256 hash: 1111\nSHA-1 hash: AB12CD\nkeychain: \"/Library/Keychains/System.keychain\"\n" +
		"version: 512\nSHA-1 hash: EF34\n"
	cases := map[string]bool{"AB12CD": true, "ab12cd": true, "EF34": true, "1111": false, "AB12": false, "": false}
	for sum, want := range cases {
		if got := keychainHasSHA1(listing, sum); got != want {
			t.Errorf("keychainHasSHA1(%q) = %v, want %v", sum, got, want)
		}
	}
}

func TestTrustListsName(t *testing.T) {
	t.Parallel()
	dump := "Number of trusted certs = 2\nCert 0: Some Root\n   Number of trust settings : 0\n" +
		"Cert 1: chainmail test\n   Number of trust settings : 1\n   Trust Setting 0:\n"
	cases := map[string]bool{"chainmail test": true, "Some Root": true, "chainmail": false, "": false}
	for name, want := range cases {
		if got := trustListsName(dump, name); got != want {
			t.Errorf("trustListsName(%q) = %v, want %v", name, got, want)
		}
	}
	if trustListsName("", "chainmail test") {
		t.Error("trustListsName found a name in an empty dump")
	}
}

func TestDeletable(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"/etc/chainmail": true, "/Library/Application Support/chainmail": true,
		"/etc": false, "/usr/local/libexec": false, "chainmail": false, "": false, "/var/lib/chainmail/..": false,
	}
	for dir, want := range cases {
		if got := deletable(dir); got != want {
			t.Errorf("deletable(%q) = %v, want %v", dir, got, want)
		}
	}
}

func TestAncestors(t *testing.T) {
	t.Parallel()
	got := ancestors("/var/lib/chainmail", "/usr/local/libexec/chainmail", "/etc/chainmail", "/var/log/chainmail")
	want := []string{"/usr/local/libexec", "/usr/local", "/var/lib", "/var/log", "/etc", "/usr", "/var"}
	if !slices.Equal(got, want) {
		t.Errorf("ancestors = %q, want %q", got, want)
	}
	if got := ancestors("/", "relative/path"); len(got) != 0 {
		t.Errorf("ancestors of the root and a relative path = %q, want none", got)
	}
}
