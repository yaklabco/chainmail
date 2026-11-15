package setup

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/yaklabco/chainmail/internal/setup/system"
)

// buildRefusal builds a plan for alice's Gmail account on h and returns
// the refusal, failing the test when Build does not refuse.
func buildRefusal(t *testing.T, p Platform, h host, req Request) *Refusal {
	t.Helper()
	cur := h.inspect(t, p)
	cur.Health = okHealth
	return asRefusal(t, mustFail(Build(LayoutFor(p), cur, req)))
}

// wantMentions fails the test unless s holds every one of subs.
func wantMentions(t *testing.T, what, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("%s %q does not mention %q", what, s, sub)
		}
	}
}

func TestBuildDaemonUserIdentity(t *testing.T) {
	t.Parallel()
	linux := LayoutFor(Linux)

	t.Run("daemon user sharing the invoking user's uid", func(t *testing.T) {
		t.Parallel()
		// Every path the daemon owns belongs to uid 1000, alice's uid. If
		// the trust check ran with 1000 trusted, it would pass.
		h := installedHostUID(Linux, aliceGmail, nil, fakeUserUID)
		cur := h.inspect(t, Linux)
		cur.Health = okHealth
		r := asRefusal(t, mustFail(Build(linux, cur, gmailRequest())))
		wantMentions(t, "refusal", r.Msg, "uid 1000", "alice")
		if r.Hint == "" {
			t.Errorf("refusal %q has no hint", r.Msg)
		}
		if err := checkTrust(linux, cur); err == nil {
			t.Error("checkTrust passed with the daemon user sharing alice's uid")
		}
	})

	t.Run("daemon user sharing the invoking user's uid on macOS", func(t *testing.T) {
		t.Parallel()
		h := installedHostUID(Darwin, aliceGmail, nil, 501)
		r := buildRefusal(t, Darwin, h, gmailRequest())
		wantMentions(t, "refusal", r.Msg, "uid 501", "alice")
	})

	t.Run("daemon user with uid 0 on an installed tree", func(t *testing.T) {
		t.Parallel()
		h := installedHostUID(Linux, aliceGmail, nil, 0)
		r := buildRefusal(t, Linux, h, gmailRequest())
		wantMentions(t, "refusal", r.Msg, "uid 0")
	})
}

func TestBuildVaultKey(t *testing.T) {
	t.Parallel()
	type keyCase struct {
		name string
		set  func(f host, l Layout, uid, gid uint32)
		want string
	}
	file := func(mode fs.FileMode, owner func(uid, gid uint32) (uint32, uint32)) func(host, Layout, uint32, uint32) {
		return func(h host, l Layout, uid, gid uint32) {
			u, g := owner(uid, gid)
			h.files.File(l.VaultKey, u, g, mode, []byte("AGE-SECRET-KEY-FAKE\n"))
		}
	}
	daemon := func(uid, gid uint32) (uint32, uint32) { return uid, gid }
	root := func(uint32, uint32) (uint32, uint32) { return 0, 0 }
	cases := []keyCase{
		{"readable by everyone", file(0o444, daemon), "0444"},
		{"readable by the group", file(0o440, daemon), "0440"},
		{"writable by its owner", file(0o600, daemon), "0600"},
		{"owned by root 0400", file(0o400, root), "uid 0"},
		{"owned by root 0600", file(0o600, root), "uid 0"},
		{"owned by alice", file(0o400, func(uint32, uint32) (uint32, uint32) { return fakeUserUID, fakeUserUID }), "owned"},
		{"in another group", file(0o400, func(uid, _ uint32) (uint32, uint32) { return uid, 0 }), "owned"},
		{"setuid", file(fs.ModeSetuid|0o400, daemon), "mode"},
		{"a symlink", func(h host, l Layout, _, _ uint32) {
			h.files.Symlink(l.VaultKey, 0, "/tmp/key")
		}, "symlink"},
		{"a directory", func(h host, l Layout, uid, gid uint32) {
			h.files.Dir(l.VaultKey, uid, gid, 0o700)
		}, "regular file"},
	}
	for _, p := range []Platform{Linux, Darwin} {
		l := LayoutFor(p)
		uid, gid := uint32(fakeLinuxDaemon), uint32(fakeLinuxGroup)
		group := "chainmail"
		if p == Darwin {
			uid, gid, group = fakeDarwinUID, fakeDarwinGID, "chainmail-clients"
		}
		for _, tc := range cases {
			t.Run(string(p)+" "+tc.name, func(t *testing.T) {
				t.Parallel()
				h := installedHost(p, aliceGmail, nil)
				tc.set(h, l, uid, gid)
				r := buildRefusal(t, p, h, gmailRequest())
				wantMentions(t, "refusal", r.Msg, l.VaultKey, tc.want)
				fix := "sudo chown " + l.DaemonUser + ":" + group + " " + l.VaultKey +
					" && sudo chmod 0400 " + l.VaultKey
				wantMentions(t, "hint", r.Hint, fix, "new key", "password")
				wantMentions(t, "refusal", r.Msg+" "+r.Hint, "read")
			})
		}
	}

	t.Run("key without a daemon user", func(t *testing.T) {
		t.Parallel()
		l := LayoutFor(Linux)
		h := freshHost(Linux)
		h.files.Dir(l.ConfigDir, 0, 0, 0o755).File(l.VaultKey, 0, 0, 0o400, []byte("k"))
		cur := h.inspect(t, Linux)
		r := asRefusal(t, mustFail(Build(l, cur, gmailRequest())))
		wantMentions(t, "refusal", r.Msg, l.VaultKey)
	})

	t.Run("a correct key is reused", func(t *testing.T) {
		t.Parallel()
		for _, p := range []Platform{Linux, Darwin} {
			l := LayoutFor(p)
			cur := installedHost(p, aliceGmail, nil).inspect(t, p)
			cur.Health = okHealth
			plan := mustBuild(t, l, cur, gmailRequest())
			if _, ok := findAction(plan, ActCreateKey); ok {
				t.Errorf("%s: plan creates a key over a correct one", p)
			}
		}
	})
}

func TestBuildDaemonUserShape(t *testing.T) {
	t.Parallel()
	linux, darwin := LayoutFor(Linux), LayoutFor(Darwin)
	user := func(h host, l Layout, edit func(u *system.User)) {
		u, _ := h.accounts.User(l.DaemonUser)
		edit(&u)
		h.accounts.AddUser(u)
	}
	cases := []struct {
		name string
		p    Platform
		edit func(h host)
		want []string
	}{
		{"linux primary group is not chainmail", Linux, func(h host) {
			user(h, linux, func(u *system.User) { u.GID = fakeClientsGID })
		}, []string{"primary group", "chainmail"}},
		{"linux chainmail group missing", Linux, func(h host) {
			h.accounts.RemoveGroup(linux.DaemonUser)
		}, []string{"group chainmail"}},
		{"linux not in chainmail-clients", Linux, func(h host) {
			user(h, linux, func(u *system.User) { u.Groups = nil })
		}, []string{"chainmail-clients"}},
		{"linux client group missing", Linux, func(h host) {
			h.accounts.RemoveGroup(linux.ClientGroup)
		}, []string{"chainmail-clients"}},
		{"linux home", Linux, func(h host) {
			user(h, linux, func(u *system.User) { u.Home = "/home/chainmail" })
		}, []string{"home", "/home/chainmail", "/var/lib/chainmail"}},
		{"linux login shell", Linux, func(h host) {
			user(h, linux, func(u *system.User) { u.Shell = "/bin/sh" })
		}, []string{"shell", "/bin/sh"}},
		{"linux chainmail group with gid 0", Linux, func(h host) {
			h.accounts.AddGroup(system.GroupInfo{Name: linux.DaemonUser, GID: 0})
			user(h, linux, func(u *system.User) { u.GID = 0 })
		}, []string{"gid 0"}},
		{"macos primary group", Darwin, func(h host) {
			user(h, darwin, func(u *system.User) { u.GID = 20 })
		}, []string{"primary group", "chainmail-clients"}},
		{"macos client group missing", Darwin, func(h host) {
			h.accounts.RemoveGroup(darwin.ClientGroup)
		}, []string{"chainmail-clients"}},
		{"macos home", Darwin, func(h host) {
			user(h, darwin, func(u *system.User) { u.Home = "/var/lib/chainmail" })
		}, []string{"home", "/var/empty"}},
		{"macos nologin is not false", Darwin, func(h host) {
			user(h, darwin, func(u *system.User) { u.Shell = "/usr/sbin/nologin" })
		}, []string{"shell", "/usr/bin/false"}},
		{"macos client group with gid 0", Darwin, func(h host) {
			h.accounts.AddGroup(system.GroupInfo{Name: darwin.ClientGroup, GID: 0})
		}, []string{"gid 0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := installedHost(tc.p, aliceGmail, nil)
			tc.edit(h)
			r := buildRefusal(t, tc.p, h, gmailRequest())
			wantMentions(t, "refusal", r.Msg, tc.want...)
			if r.Hint == "" {
				t.Errorf("refusal %q has no hint", r.Msg)
			}
		})
	}

	t.Run("linux shells that cannot log in", func(t *testing.T) {
		t.Parallel()
		for _, sh := range []string{"/usr/sbin/nologin", "/sbin/nologin", "/bin/false", "/usr/bin/false"} {
			h := installedHost(Linux, aliceGmail, nil)
			user(h, linux, func(u *system.User) { u.Shell = sh })
			cur := h.inspect(t, Linux)
			cur.Health = okHealth
			if _, err := Build(linux, cur, gmailRequest()); err != nil {
				t.Errorf("shell %s: %v", sh, err)
			}
		}
	})
}

func TestBuildConfigDaemonTable(t *testing.T) {
	t.Parallel()
	withDaemon := func(table string) string {
		return "[daemon]\n" + table + "\n\n" + handWritten
	}
	cases := []struct {
		name   string
		p      Platform
		config string
		want   string // "" means Build plans without a refusal
	}{
		{"linux socket elsewhere", Linux, withDaemon(`socket = "/tmp/evil.sock"`), "socket"},
		{"linux client group elsewhere", Linux, withDaemon(`client_group = "users"`), "client_group"},
		{"linux socket spelled out", Linux, withDaemon(`socket = "/run/chainmail/chainmail.sock"`), ""},
		{"linux defaults", Linux, handWritten, ""},
		{"macos socket left at the Linux default", Darwin, handWritten, "socket"},
		{"macos socket elsewhere", Darwin, withDaemon(`socket = "/tmp/evil.sock"`), "socket"},
		{"macos layout socket", Darwin, withDaemon(`socket = "/var/db/chainmail/chainmail.sock"`), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := LayoutFor(tc.p)
			h := installedHost(tc.p, aliceGmail, nil)
			h.files.File(l.Config, 0, 0, 0o644, []byte(tc.config))
			cur := h.inspect(t, tc.p)
			_, err := Build(l, cur, gmailRequest())
			if tc.want == "" {
				if err != nil {
					t.Errorf("Build: %v", err)
				}
				return
			}
			r := asRefusal(t, err)
			wantMentions(t, "refusal", r.Msg, tc.want, l.Config)
		})
	}
}

func TestBuildBackupShowNeedsTerminal(t *testing.T) {
	t.Parallel()
	req := gmailRequest()
	req.Backup = BackupShow
	req.Terminal = false

	t.Run("new key", func(t *testing.T) {
		t.Parallel()
		r := asRefusal(t, mustFail(Build(LayoutFor(Linux), freshHost(Linux).inspect(t, Linux), req)))
		if r.Msg != "--key-backup show needs a terminal; captured output would hold the key" {
			t.Errorf("refusal = %q", r.Msg)
		}
		if r.Hint == "" {
			t.Error("refusal has no hint")
		}
	})

	t.Run("reused key", func(t *testing.T) {
		t.Parallel()
		cur := installedHost(Linux, aliceGmail, nil).inspect(t, Linux)
		cur.Health = okHealth
		mustBuild(t, LayoutFor(Linux), cur, req)
	})
}
