package setup

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/yaklabco/chainmail/internal/provider"
)

// aliceGmail is the account most tests install.
var aliceGmail = AccountSpec{Name: "alice", Provider: "gmail", Address: "alice@gmail.com", Auth: provider.AuthPassword}

func TestInspect(t *testing.T) {
	t.Parallel()

	t.Run("fresh linux", func(t *testing.T) {
		t.Parallel()
		cur := freshHost(Linux).inspect(t, Linux)
		if cur.Platform != Linux {
			t.Errorf("Platform = %q, want linux", cur.Platform)
		}
		if cur.NixOS || cur.DaemonUser != nil || cur.ClientGroup != nil ||
			cur.Config != nil || cur.ConfigMarked || cur.Service != nil || cur.ServiceMarked ||
			cur.KeyExists || cur.VaultExists || cur.BinarySHA256 != "" || cur.Health != nil {
			t.Errorf("fresh host: got %+v, want everything nil or false", cur)
		}
		if cur.InvokingUser.Name != "alice" || cur.InvokingUser.UID != fakeUserUID {
			t.Errorf("InvokingUser = %+v, want alice", cur.InvokingUser)
		}
	})

	for _, p := range []Platform{Linux, Darwin} {
		t.Run("full install "+string(p), func(t *testing.T) {
			t.Parallel()
			l := LayoutFor(p)
			cur := installedHost(p, aliceGmail, nil).inspect(t, p)
			if cur.DaemonUser == nil || cur.DaemonUser.Name != l.DaemonUser {
				t.Errorf("DaemonUser = %+v, want %s", cur.DaemonUser, l.DaemonUser)
			}
			if cur.ClientGroup == nil || cur.ClientGroup.Name != l.ClientGroup {
				t.Errorf("ClientGroup = %+v, want %s", cur.ClientGroup, l.ClientGroup)
			}
			if string(cur.Config) != string(NewConfig(l, aliceGmail, nil)) || !cur.ConfigMarked {
				t.Errorf("Config = %q marked %v, want the bootstrap config, marked", cur.Config, cur.ConfigMarked)
			}
			if string(cur.Service) != string(RenderService(l)) || !cur.ServiceMarked {
				t.Errorf("Service marked %v, want the rendered service, marked", cur.ServiceMarked)
			}
			if !cur.KeyExists || !cur.VaultExists {
				t.Errorf("KeyExists %v VaultExists %v, want both true", cur.KeyExists, cur.VaultExists)
			}
			if cur.BinarySHA256 != fakeBinarySHA {
				t.Errorf("BinarySHA256 = %q, want %q", cur.BinarySHA256, fakeBinarySHA)
			}
			if cur.Health != nil {
				t.Errorf("Health = %+v, want nil: the CLI fills it in", cur.Health)
			}
		})
	}

	t.Run("markers follow HasMarker", func(t *testing.T) {
		t.Parallel()
		l := LayoutFor(Linux)
		h := installedHost(Linux, aliceGmail, nil)
		h.files.file(l.Config, 0, 0, 0o644, []byte(handWritten))
		h.files.file(l.Service, 0, 0, 0o644, []byte(deployPageUnit))
		cur := h.inspect(t, Linux)
		if cur.ConfigMarked || cur.ServiceMarked {
			t.Errorf("ConfigMarked %v ServiceMarked %v, want false for hand-written files", cur.ConfigMarked, cur.ServiceMarked)
		}
		if string(cur.Config) != handWritten {
			t.Errorf("Config = %q, want the hand-written config", cur.Config)
		}
	})

	t.Run("nixos", func(t *testing.T) {
		t.Parallel()
		h := freshHost(Linux)
		h.files.file("/etc/NIXOS", 0, 0, 0o644, nil)
		if cur := h.inspect(t, Linux); !cur.NixOS {
			t.Error("NixOS = false with /etc/NIXOS present")
		}
	})

	t.Run("config symlink is not read", func(t *testing.T) {
		t.Parallel()
		l := LayoutFor(Linux)
		h := installedHost(Linux, aliceGmail, nil)
		h.files.symlink(l.Config, 0, "/nix/store/config.toml")
		cur := h.inspect(t, Linux)
		if cur.Config != nil {
			t.Errorf("Config = %q, want nil for a symlink", cur.Config)
		}
	})

	t.Run("unreadable state directory", func(t *testing.T) {
		t.Parallel()
		// As the invoking user, /var/lib/chainmail is mode 0700 and owned by
		// the daemon, so the vault cannot even be looked at.
		l := LayoutFor(Linux)
		h := installedHost(Linux, aliceGmail, nil)
		h.files.errs[l.Vault] = fs.ErrPermission
		cur := h.inspect(t, Linux)
		if cur.VaultExists {
			t.Error("VaultExists = true for a vault that cannot be looked at")
		}
		if _, err := Build(l, cur, gmailRequest()); err != nil {
			t.Errorf("Build after a permission error: %v", err)
		}
	})

	t.Run("lstat error", func(t *testing.T) {
		t.Parallel()
		l := LayoutFor(Linux)
		h := installedHost(Linux, aliceGmail, nil)
		boom := errors.New("disk on fire")
		h.files.errs[l.VaultKey] = boom
		_, err := Inspect(context.Background(), l, h.files, h.accounts, "alice")
		if !errors.Is(err, boom) {
			t.Errorf("Inspect error = %v, want it to wrap %v", err, boom)
		}
	})

	t.Run("missing invoking user", func(t *testing.T) {
		t.Parallel()
		h := freshHost(Linux)
		if _, err := Inspect(context.Background(), LayoutFor(Linux), h.files, h.accounts, "mallory"); err == nil {
			t.Error("Inspect for an unknown user succeeded")
		}
	})
}
