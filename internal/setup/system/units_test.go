package system

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const showUnit = "systemctl show chainmail.service -p LoadState -p FragmentPath -p ExecStart -p ActiveState"

// incusExecStart is the ExecStart line systemctl show prints for the
// module's unit on incus, with the store hashes shortened.
const incusExecStart = "ExecStart={ path=/nix/store/afs-chainmail-3ebad8d/bin/chainmail ; " +
	"argv[]=/nix/store/afs-chainmail-3ebad8d/bin/chainmail daemon --config /etc/chainmail/config.toml ; " +
	"ignore_errors=no ; start_time=[Wed 2026-11-11 08:23:31 PST] ; stop_time=[n/a] ; " +
	"pid=2713115 ; code=(null) ; status=0/0 }"

func TestUnitsShow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// The incus layout: the unit file in /etc is a link into the store.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	storeUnit := filepath.Join(root, "nix/store/x-unit/chainmail.service")
	etcUnit := filepath.Join(root, "etc/systemd/system/chainmail.service")
	for _, dir := range []string{filepath.Dir(storeUnit), filepath.Dir(etcUnit)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(storeUnit, []byte("[Service]\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(storeUnit, etcUnit); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		out  string
		want UnitInfo
	}{
		{
			// systemctl show prints properties in its own order.
			name: "module unit on incus",
			out: "FragmentPath=" + etcUnit + "\n" +
				incusExecStart + "\n" +
				"LoadState=loaded\n" +
				"ActiveState=active\n",
			want: UnitInfo{
				Loaded:    true,
				LoadState: "loaded",
				Fragment:  storeUnit,
				ExecPath:  "/nix/store/afs-chainmail-3ebad8d/bin/chainmail",
				Active:    true,
			},
		},
		{
			name: "no unit",
			out:  "LoadState=not-found\nFragmentPath=\nExecStart=\nActiveState=inactive\n",
			want: UnitInfo{LoadState: "not-found"},
		},
		{
			name: "fragment missing",
			out:  "LoadState=loaded\nFragmentPath=" + filepath.Join(root, "etc/missing.service") + "\nExecStart=\nActiveState=inactive\n",
			want: UnitInfo{Loaded: true, LoadState: "loaded"},
		},
		{
			name: "exec start without path",
			out:  "LoadState=loaded\nFragmentPath=\nExecStart={ argv[]=/bin/true ; ignore_errors=no }\nActiveState=active\n",
			want: UnitInfo{Loaded: true, LoadState: "loaded", Active: true},
		},
		{
			name: "exec start path without terminator",
			out:  "LoadState=loaded\nExecStart={ path=/bin/true\nActiveState=active\n",
			want: UnitInfo{Loaded: true, LoadState: "loaded", Active: true},
		},
		{
			name: "first exec start entry",
			out: "LoadState=loaded\n" +
				"ExecStart={ path=/bin/first ; argv[]=/bin/first ; ignore_errors=no }\n" +
				"ExecStart={ path=/bin/second ; argv[]=/bin/second ; ignore_errors=no }\n",
			want: UnitInfo{Loaded: true, LoadState: "loaded", ExecPath: "/bin/first"},
		},
		{
			name: "failed unit",
			out:  "LoadState=loaded\nActiveState=failed\n",
			want: UnitInfo{Loaded: true, LoadState: "loaded"},
		},
		{
			// A unit file systemd rejects keeps its fragment but is not
			// loaded.
			name: "module unit with a bad setting",
			out:  "LoadState=bad-setting\nFragmentPath=" + etcUnit + "\nExecStart=\nActiveState=inactive\n",
			want: UnitInfo{LoadState: "bad-setting", Fragment: storeUnit},
		},
		{
			name: "module unit that failed to load",
			out:  "LoadState=error\nFragmentPath=" + etcUnit + "\nExecStart=\nActiveState=inactive\n",
			want: UnitInfo{LoadState: "error", Fragment: storeUnit},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newFakeRunner().on(showUnit, tt.out, nil)
			got, err := NewUnits(r.run).Show(ctx, "chainmail.service")
			if err != nil {
				t.Fatalf("Show: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Show = %+v, want %+v", got, tt.want)
			}
			r.wantCalls(t, showUnit)
		})
	}

	t.Run("runner error", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().onFail(showUnit, "", 1, "no bus")
		got, err := NewUnits(r.run).Show(ctx, "chainmail.service")
		var exit *fakeExit
		if !errors.As(err, &exit) || !strings.Contains(err.Error(), "no bus") {
			t.Fatalf("Show error = %v, want the runner's error wrapped", err)
		}
		if !strings.Contains(err.Error(), "chainmail.service") {
			t.Fatalf("Show error %q does not name the unit", err)
		}
		if got != (UnitInfo{}) {
			t.Fatalf("Show on error = %+v, want the zero UnitInfo", got)
		}
	})
}
