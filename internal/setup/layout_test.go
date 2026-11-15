package setup

import (
	"io/fs"
	"slices"
	"testing"
)

// The tests below are the pinned copy of the spec's Part 2 table. Changing a
// path, owner or mode needs a deliberate edit here.

func TestLayoutLinux(t *testing.T) {
	t.Parallel()
	got := LayoutFor(Linux)
	want := Layout{
		Platform:     Linux,
		DaemonUser:   "chainmail",
		ClientGroup:  "chainmail-clients",
		RootGroup:    "root",
		BinaryDir:    "/usr/local/libexec/chainmail",
		Binary:       "/usr/local/libexec/chainmail/chainmail",
		ConfigDir:    "/etc/chainmail",
		Config:       "/etc/chainmail/config.toml",
		VaultKey:     "/etc/chainmail/vault-key",
		Lock:         "/etc/chainmail/.bootstrap.lock",
		StateDir:     "/var/lib/chainmail",
		Vault:        "/var/lib/chainmail/vault.age",
		SocketDir:    "/run/chainmail",
		Socket:       "/run/chainmail/chainmail.sock",
		Service:      "/etc/systemd/system/chainmail.service",
		ServiceLabel: "chainmail.service",
		LogDir:       "",
		LogFile:      "",
	}
	if got != want {
		t.Errorf("LayoutFor(Linux) =\n%+v\nwant\n%+v", got, want)
	}

	wantEntries := []Entry{
		{Name: "binary-dir", Path: "/usr/local/libexec/chainmail", Kind: KindDir, Owner: OwnerRoot, Group: GroupRoot, Mode: 0o755},
		{Name: "binary", Path: "/usr/local/libexec/chainmail/chainmail", Kind: KindFile, Owner: OwnerRoot, Group: GroupRoot, Mode: 0o755},
		{Name: "config-dir", Path: "/etc/chainmail", Kind: KindDir, Owner: OwnerRoot, Group: GroupRoot, Mode: 0o755},
		{Name: "config", Path: "/etc/chainmail/config.toml", Kind: KindFile, Owner: OwnerRoot, Group: GroupRoot, Mode: 0o644},
		{Name: "vault-key", Path: "/etc/chainmail/vault-key", Kind: KindFile, Owner: OwnerDaemon, Group: GroupDaemon, Mode: 0o400},
		{Name: "state-dir", Path: "/var/lib/chainmail", Kind: KindDir, Owner: OwnerDaemon, Group: GroupDaemon, Mode: 0o700},
		{Name: "service", Path: "/etc/systemd/system/chainmail.service", Kind: KindFile, Owner: OwnerRoot, Group: GroupRoot, Mode: 0o644},
	}
	assertEntries(t, got.Entries(), wantEntries)
}

func TestLayoutDarwin(t *testing.T) {
	t.Parallel()
	got := LayoutFor(Darwin)
	want := Layout{
		Platform:     Darwin,
		DaemonUser:   "_chainmail",
		ClientGroup:  "chainmail-clients",
		RootGroup:    "wheel",
		BinaryDir:    "/Library/Application Support/chainmail/bin",
		Binary:       "/Library/Application Support/chainmail/bin/chainmail",
		ConfigDir:    "/etc/chainmail",
		Config:       "/etc/chainmail/config.toml",
		VaultKey:     "/etc/chainmail/vault-key",
		Lock:         "/etc/chainmail/.bootstrap.lock",
		StateDir:     "/var/lib/chainmail",
		Vault:        "/var/lib/chainmail/vault.age",
		SocketDir:    "/var/db/chainmail",
		Socket:       "/var/db/chainmail/chainmail.sock",
		Service:      "/Library/LaunchDaemons/sh.chainmail.daemon.plist",
		ServiceLabel: "sh.chainmail.daemon",
		LogDir:       "/var/log/chainmail",
		LogFile:      "/var/log/chainmail/daemon.log",
	}
	if got != want {
		t.Errorf("LayoutFor(Darwin) =\n%+v\nwant\n%+v", got, want)
	}

	wantEntries := []Entry{
		{Name: "binary-dir", Path: "/Library/Application Support/chainmail/bin", Kind: KindDir, Owner: OwnerRoot, Group: GroupRoot, Mode: 0o755},
		{Name: "binary", Path: "/Library/Application Support/chainmail/bin/chainmail", Kind: KindFile, Owner: OwnerRoot, Group: GroupRoot, Mode: 0o755},
		{Name: "config-dir", Path: "/etc/chainmail", Kind: KindDir, Owner: OwnerRoot, Group: GroupRoot, Mode: 0o755},
		{Name: "config", Path: "/etc/chainmail/config.toml", Kind: KindFile, Owner: OwnerRoot, Group: GroupRoot, Mode: 0o644},
		{Name: "vault-key", Path: "/etc/chainmail/vault-key", Kind: KindFile, Owner: OwnerDaemon, Group: GroupDaemon, Mode: 0o400},
		{Name: "state-dir", Path: "/var/lib/chainmail", Kind: KindDir, Owner: OwnerDaemon, Group: GroupDaemon, Mode: 0o700},
		{Name: "socket-dir", Path: "/var/db/chainmail", Kind: KindDir, Owner: OwnerDaemon, Group: GroupDaemon, Mode: 0o755},
		{Name: "log-dir", Path: "/var/log/chainmail", Kind: KindDir, Owner: OwnerDaemon, Group: GroupDaemon, Mode: 0o700},
		{Name: "service", Path: "/Library/LaunchDaemons/sh.chainmail.daemon.plist", Kind: KindFile, Owner: OwnerRoot, Group: GroupRoot, Mode: 0o644},
	}
	assertEntries(t, got.Entries(), wantEntries)
}

func assertEntries(t *testing.T, got, want []Entry) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("Entries() has %d rows, want %d", len(got), len(want))
	}
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			t.Errorf("Entries()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLayoutForUnknownPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("LayoutFor(\"plan9\") did not panic")
		}
	}()
	LayoutFor(Platform("plan9"))
}

func TestEntryLookup(t *testing.T) {
	t.Parallel()
	for _, p := range []Platform{Linux, Darwin} {
		l := LayoutFor(p)
		for _, want := range l.Entries() {
			got, ok := l.Entry(want.Name)
			if !ok || got != want {
				t.Errorf("%s: Entry(%q) = %+v, %v, want %+v, true", p, want.Name, got, ok, want)
			}
		}
		if _, ok := l.Entry("nope"); ok {
			t.Errorf("%s: Entry(\"nope\") found an entry", p)
		}
	}
	if _, ok := LayoutFor(Linux).Entry("socket-dir"); ok {
		t.Error("Linux has a socket-dir entry; systemd creates /run/chainmail")
	}
	if _, ok := LayoutFor(Linux).Entry("log-dir"); ok {
		t.Error("Linux has a log-dir entry; logs go to journald")
	}
}

func TestEntriesReturnsCopy(t *testing.T) {
	t.Parallel()
	l := LayoutFor(Linux)
	e := l.Entries()
	e[0].Path = "/tmp/evil"
	if got, _ := l.Entry("binary-dir"); got.Path != "/usr/local/libexec/chainmail" {
		t.Errorf("mutating Entries() changed the layout: %q", got.Path)
	}
}

func TestServiceArgs(t *testing.T) {
	t.Parallel()
	want := []string{
		"daemon",
		"--config", "/etc/chainmail/config.toml",
		"--state-dir", "/var/lib/chainmail",
		"--vault-key-file", "/etc/chainmail/vault-key",
	}
	for _, p := range []Platform{Linux, Darwin} {
		got := LayoutFor(p).ServiceArgs()
		if !slices.Equal(got, want) {
			t.Errorf("%s: ServiceArgs() = %q, want %q", p, got, want)
		}
		if slices.Contains(got, "--dev") {
			t.Errorf("%s: ServiceArgs() contains --dev", p)
		}
	}
}

func TestAllowed(t *testing.T) {
	t.Parallel()
	for _, p := range []Platform{Linux, Darwin} {
		l := LayoutFor(p)
		allowed := []string{l.Socket, l.Lock, l.Vault}
		for _, e := range l.Entries() {
			allowed = append(allowed, e.Path)
		}
		for _, path := range allowed {
			if !l.Allowed(path) {
				t.Errorf("%s: Allowed(%q) = false, want true", p, path)
			}
		}
		// Paths are compared after filepath.Clean.
		if !l.Allowed("/etc/chainmail//config.toml") {
			t.Errorf("%s: Allowed of an unclean config path = false, want true", p)
		}
		refused := []string{
			"/etc/chainmail/../passwd",
			"/etc/chainmail/../chainmail/config.toml",
			"/etc/chainmail/config.toml.bak",
			"/tmp/x",
			"",
			"etc/chainmail/config.toml",
			"/",
		}
		for _, path := range refused {
			if l.Allowed(path) {
				t.Errorf("%s: Allowed(%q) = true, want false", p, path)
			}
		}
	}
}

// The modes in the table are permission bits only.
func TestEntryModesArePermBits(t *testing.T) {
	t.Parallel()
	for _, p := range []Platform{Linux, Darwin} {
		for _, e := range LayoutFor(p).Entries() {
			if e.Mode&^fs.ModePerm != 0 {
				t.Errorf("%s: %s mode %v has non-permission bits", p, e.Name, e.Mode)
			}
		}
	}
}
