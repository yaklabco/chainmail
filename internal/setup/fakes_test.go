package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"

	"github.com/yaklabco/chainmail/internal/setup/setuptest"
	"github.com/yaklabco/chainmail/internal/setup/system"
)

// Fake ids used by the host fakes.
const (
	fakeUserUID     = setuptest.AliceLinuxUID
	fakeLinuxDaemon = 990
	fakeLinuxGroup  = 989
	fakeDarwinUID   = 300
	fakeClientsGID  = 988
	fakeDarwinGID   = 301
)

// fakeBinary is the installed binary's contents in the host fakes.
var fakeBinary = []byte("chainmail binary v1")

// fakeBinarySHA is the hex SHA-256 of fakeBinary.
var fakeBinarySHA = sha256Hex(fakeBinary)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// host is a fake machine: its files and its users and groups.
type host struct {
	files    *setuptest.Files
	accounts *setuptest.Accounts
}

// freshHost is a machine with no chainmail on it and one login user, alice.
func freshHost(p Platform) host {
	h := setuptest.FreshHost(string(p))
	return host{files: h.Files, accounts: h.Accounts}
}

// installedHost is a machine bootstrap already set up for spec, with
// alice in the client group and the binary from fakeBinary.
func installedHost(p Platform, spec AccountSpec, client *OAuthClientSpec) host {
	h := freshHost(p)
	l := LayoutFor(p)
	daemonUID, daemonGID, clientsGID := uint32(fakeLinuxDaemon), uint32(fakeLinuxGroup), uint32(fakeClientsGID)
	daemonUser := system.User{Name: l.DaemonUser, UID: daemonUID, GID: daemonGID, Home: l.StateDir, Shell: "/usr/sbin/nologin", Groups: []string{l.ClientGroup}}
	if p == Darwin {
		daemonUID, daemonGID, clientsGID = fakeDarwinUID, fakeDarwinGID, fakeDarwinGID
		daemonUser = system.User{Name: l.DaemonUser, UID: daemonUID, GID: clientsGID, Home: "/var/empty", Shell: "/usr/bin/false"}
	}
	h.accounts.AddUser(daemonUser)
	h.accounts.AddGroup(system.GroupInfo{Name: l.ClientGroup, GID: clientsGID, Members: []string{"alice"}})
	if p == Linux {
		h.accounts.AddGroup(system.GroupInfo{Name: l.DaemonUser, GID: daemonGID})
	}
	alice, _ := h.accounts.User("alice")
	alice.Groups = append(alice.Groups, l.ClientGroup)
	h.accounts.AddUser(alice)

	f := h.files
	data := map[string][]byte{
		"binary":    fakeBinary,
		"config":    NewConfig(l, spec, client),
		"vault-key": []byte("AGE-SECRET-KEY-FAKE\n"),
		"service":   RenderService(l),
	}
	for _, e := range l.Entries() {
		uid, gid := uint32(0), uint32(0)
		if e.Owner == OwnerDaemon {
			uid = daemonUID
		}
		if e.Group == GroupDaemon {
			gid = daemonGID
			if p == Darwin {
				gid = clientsGID
			}
		}
		if e.Kind == KindDir {
			f.Dir(e.Path, uid, gid, e.Mode)
			continue
		}
		f.File(e.Path, uid, gid, e.Mode, data[e.Name])
	}
	if p == Linux {
		f.Dir(l.SocketDir, daemonUID, daemonGID, 0o755)
	}
	f.File(l.Socket, daemonUID, clientsGID, 0o660|fs.ModeSocket, nil)
	f.File(l.Vault, daemonUID, daemonGID, 0o600, []byte("vault"))
	if p == Darwin {
		f.File(l.LogFile, daemonUID, clientsGID, 0o600, nil)
	}
	return h
}

// inspect runs Inspect on h for alice and fails the test on error.
func (h host) inspect(t interface {
	Helper()
	Fatalf(string, ...any)
}, p Platform,
) Current {
	t.Helper()
	cur, err := Inspect(context.Background(), LayoutFor(p), h.files, h.accounts, "alice")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	return cur
}
