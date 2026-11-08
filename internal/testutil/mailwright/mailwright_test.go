//go:build caldav

package mailwright

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRunArgs(t *testing.T) {
	exp := time.Unix(1800000000, 0)
	args := runArgs("chainmail-mailwright-x", "/tmp/d", "img@sha256:abc", 20*time.Minute, exp)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--label chainmail-test=mailwright",
		"--label chainmail-test.expires=1800000000",
		"-p 127.0.0.1::8080",
		"-v /tmp/d/config.toml:/etc/mailwright/config.toml:ro",
		"--init",
		"--entrypoint /usr/bin/timeout img@sha256:abc -k 10s 1200s /usr/local/bin/mailwright",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("run args lack %q: %s", want, joined)
		}
	}
	if slices.Contains(args, "--rm") {
		t.Error("--rm would lose the log of a container that dies at startup")
	}
}

func TestConfigNamesNoUnknownSections(t *testing.T) {
	for _, want := range []string{`protocol = "http"`, `type = "rocksdb"`, "[authentication.fallback-admin]", "min-strength = 0", "enable = false"} {
		if !strings.Contains(config, want) {
			t.Errorf("config lacks %q", want)
		}
	}
	if strings.Count(config, "%s") != 1 {
		t.Error("config must take exactly the admin password")
	}
}

func TestStaleContainersAndLifetime(t *testing.T) {
	now := time.Unix(1000, 0)
	got := staleContainers("old 900\nlive 2000\nbad x\n\n", now)
	if strings.Join(got, " ") != "old bad" {
		t.Errorf("stale = %v", got)
	}
	if lifetimeFor(20*time.Minute, 0) != 20*time.Minute {
		t.Error("no timeout keeps the base lifetime")
	}
	if got := lifetimeFor(20*time.Minute, time.Hour); got <= time.Hour {
		t.Errorf("lifetime %v does not outlast the timeout", got)
	}
}

func TestRunMain(t *testing.T) {
	srv := &Server{URL: "http://127.0.0.1:1"}
	stops := 0
	stop := func(*Server) error { stops++; return nil }
	sigs := make(chan os.Signal)
	code := runMain(func() (*Server, error) { return srv, nil }, stop, func() int {
		if Shared() != srv {
			t.Error("Shared is not the running server")
		}
		return 0
	}, sigs, func(int) { t.Error("exit called") })
	if code != 0 || stops != 1 || Shared() != nil {
		t.Errorf("code %d stops %d shared %v", code, stops, Shared())
	}
	code = runMain(func() (*Server, error) { return nil, errors.New("no docker") }, stop, func() int {
		t.Error("tests ran without a server")
		return 0
	}, sigs, func(int) {})
	if code != 1 {
		t.Errorf("failed start: code %d", code)
	}
}

func TestRequireSkipsWithoutServer(t *testing.T) {
	if Shared() != nil {
		t.Skip("a server is running")
	}
	ran := false
	t.Run("inner", func(t *testing.T) {
		defer func() { ran = t.Skipped() }()
		Require(t)
	})
	if !ran {
		t.Error("Require did not skip")
	}
}
