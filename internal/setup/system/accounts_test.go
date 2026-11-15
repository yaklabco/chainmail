package system

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const (
	linuxPasswd = "chainmail:x:995:995::/var/lib/chainmail:/usr/sbin/nologin\n"
	linuxGroup  = "chainmail-clients:x:994:alice,chainmail\n"
	dsNotFound  = "<dscl_cmd> DS Error: -14136 (eDSRecordNotFound)"
)

func TestAccountsLinux(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("LookupUser", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().
			on("getent passwd chainmail", linuxPasswd, nil).
			on("id -Gn chainmail", "chainmail chainmail-clients\n", nil)
		u, ok, err := NewAccounts("linux", r.run).LookupUser(ctx, "chainmail")
		if err != nil || !ok {
			t.Fatalf("LookupUser = %v, %v", ok, err)
		}
		want := User{
			Name: "chainmail", UID: 995, GID: 995,
			Home: "/var/lib/chainmail", Shell: "/usr/sbin/nologin",
			Groups: []string{"chainmail-clients"},
		}
		if !reflect.DeepEqual(u, want) {
			t.Fatalf("user = %+v, want %+v", u, want)
		}
		r.wantCalls(t, "getent passwd chainmail", "id -Gn chainmail")
	})

	t.Run("LookupUser missing", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on("getent passwd chainmail", "", &fakeExit{code: 2})
		_, ok, err := NewAccounts("linux", r.run).LookupUser(ctx, "chainmail")
		if err != nil || ok {
			t.Fatalf("LookupUser = %v, %v, want false, nil", ok, err)
		}
		r.wantCalls(t, "getent passwd chainmail")
	})

	t.Run("LookupUser failure", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on("getent passwd chainmail", "", &fakeExit{code: 1})
		if _, _, err := NewAccounts("linux", r.run).LookupUser(ctx, "chainmail"); err == nil {
			t.Fatal("LookupUser succeeded when getent failed")
		}
	})

	t.Run("LookupUser malformed", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on("getent passwd chainmail", "chainmail:x:nope\n", nil)
		if _, _, err := NewAccounts("linux", r.run).LookupUser(ctx, "chainmail"); err == nil {
			t.Fatal("LookupUser accepted malformed passwd output")
		}
	})

	t.Run("LookupGroup", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on("getent group chainmail-clients", linuxGroup, nil)
		g, ok, err := NewAccounts("linux", r.run).LookupGroup(ctx, "chainmail-clients")
		if err != nil || !ok {
			t.Fatalf("LookupGroup = %v, %v", ok, err)
		}
		want := GroupInfo{Name: "chainmail-clients", GID: 994, Members: []string{"alice", "chainmail"}}
		if !reflect.DeepEqual(g, want) {
			t.Fatalf("group = %+v, want %+v", g, want)
		}
	})

	t.Run("LookupGroup no members", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on("getent group chainmail-clients", "chainmail-clients:x:994:\n", nil)
		g, ok, err := NewAccounts("linux", r.run).LookupGroup(ctx, "chainmail-clients")
		if err != nil || !ok {
			t.Fatalf("LookupGroup = %v, %v", ok, err)
		}
		if len(g.Members) != 0 {
			t.Fatalf("members = %q, want none", g.Members)
		}
	})

	t.Run("LookupGroup missing", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on("getent group chainmail-clients", "", &fakeExit{code: 2})
		_, ok, err := NewAccounts("linux", r.run).LookupGroup(ctx, "chainmail-clients")
		if err != nil || ok {
			t.Fatalf("LookupGroup = %v, %v, want false, nil", ok, err)
		}
	})

	t.Run("CreateGroup", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on("getent group chainmail-clients", "chainmail-clients:x:994:\n", nil)
		g, err := NewAccounts("linux", r.run).CreateGroup(ctx, "chainmail-clients")
		if err != nil {
			t.Fatalf("CreateGroup: %v", err)
		}
		if g.Name != "chainmail-clients" || g.GID != 994 {
			t.Fatalf("group = %+v", g)
		}
		r.wantCalls(t, "groupadd --system chainmail-clients", "getent group chainmail-clients")
	})

	t.Run("CreateGroup failure", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on("groupadd --system chainmail-clients", "", &fakeExit{code: 9, stderr: "exists"})
		if _, err := NewAccounts("linux", r.run).CreateGroup(ctx, "chainmail-clients"); err == nil {
			t.Fatal("CreateGroup succeeded when groupadd failed")
		}
		r.wantCalls(t, "groupadd --system chainmail-clients")
	})

	t.Run("CreateDaemonUser", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().
			on("getent passwd chainmail", linuxPasswd, nil).
			on("id -Gn chainmail", "chainmail chainmail-clients\n", nil)
		u, err := NewAccounts("linux", r.run).CreateDaemonUser(ctx, "chainmail", "chainmail-clients", "/var/lib/chainmail")
		if err != nil {
			t.Fatalf("CreateDaemonUser: %v", err)
		}
		if u.Name != "chainmail" || u.UID != 995 {
			t.Fatalf("user = %+v", u)
		}
		r.wantCalls(t,
			"useradd --system --user-group --groups chainmail-clients --home-dir /var/lib/chainmail --shell /usr/sbin/nologin chainmail",
			"getent passwd chainmail",
			"id -Gn chainmail",
		)
	})

	t.Run("AddMember", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner()
		if err := NewAccounts("linux", r.run).AddMember(ctx, "chainmail-clients", "alice"); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
		r.wantCalls(t, "usermod -aG chainmail-clients alice")
	})
}

const plistHead = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`

const darwinUserPlist = plistHead + `	<key>dsAttrTypeStandard:NFSHomeDirectory</key>
	<array>
		<string>/var/empty</string>
	</array>
	<key>dsAttrTypeStandard:PrimaryGroupID</key>
	<array>
		<string>302</string>
	</array>
	<key>dsAttrTypeStandard:UniqueID</key>
	<array>
		<string>301</string>
	</array>
	<key>dsAttrTypeStandard:UserShell</key>
	<array>
		<string>/usr/bin/false</string>
	</array>
</dict>
</plist>
`

const darwinGroupPlist = plistHead + `	<key>dsAttrTypeStandard:GroupMembership</key>
	<array>
		<string>alice</string>
		<string>bob</string>
	</array>
	<key>dsAttrTypeStandard:PrimaryGroupID</key>
	<array>
		<string>302</string>
	</array>
</dict>
</plist>
`

const darwinEmptyGroupPlist = plistHead + `	<key>dsAttrTypeStandard:PrimaryGroupID</key>
	<array>
		<string>302</string>
	</array>
</dict>
</plist>
`

const (
	dsReadUser  = "dscl -plist . -read /Users/_chainmail UniqueID PrimaryGroupID NFSHomeDirectory UserShell"
	dsReadGroup = "dscl -plist . -read /Groups/chainmail-clients PrimaryGroupID GroupMembership"
)

func TestAccountsDarwin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("LookupUser", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().
			on(dsReadUser, darwinUserPlist, nil).
			on("id -Gn _chainmail", "chainmail-clients everyone localaccounts\n", nil)
		u, ok, err := NewAccounts("darwin", r.run).LookupUser(ctx, "_chainmail")
		if err != nil || !ok {
			t.Fatalf("LookupUser = %v, %v", ok, err)
		}
		want := User{
			Name: "_chainmail", UID: 301, GID: 302,
			Home: "/var/empty", Shell: "/usr/bin/false",
			Groups: []string{"everyone", "localaccounts"},
		}
		if !reflect.DeepEqual(u, want) {
			t.Fatalf("user = %+v, want %+v", u, want)
		}
		r.wantCalls(t, dsReadUser, "id -Gn _chainmail")
	})

	t.Run("LookupUser missing", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on(dsReadUser, "", &fakeExit{code: 56, stderr: dsNotFound})
		_, ok, err := NewAccounts("darwin", r.run).LookupUser(ctx, "_chainmail")
		if err != nil || ok {
			t.Fatalf("LookupUser = %v, %v, want false, nil", ok, err)
		}
		r.wantCalls(t, dsReadUser)
	})

	t.Run("LookupUser failure", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on(dsReadUser, "", &fakeExit{code: 1, stderr: "eDSPermissionError"})
		if _, _, err := NewAccounts("darwin", r.run).LookupUser(ctx, "_chainmail"); err == nil {
			t.Fatal("LookupUser succeeded when dscl failed")
		}
	})

	t.Run("LookupGroup", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on(dsReadGroup, darwinGroupPlist, nil)
		g, ok, err := NewAccounts("darwin", r.run).LookupGroup(ctx, "chainmail-clients")
		if err != nil || !ok {
			t.Fatalf("LookupGroup = %v, %v", ok, err)
		}
		want := GroupInfo{Name: "chainmail-clients", GID: 302, Members: []string{"alice", "bob"}}
		if !reflect.DeepEqual(g, want) {
			t.Fatalf("group = %+v, want %+v", g, want)
		}
	})

	t.Run("LookupGroup no members", func(t *testing.T) {
		t.Parallel()
		// dscl reports the missing GroupMembership key on stderr and exits
		// non-zero, but still prints the attributes it found.
		r := newFakeRunner().on(dsReadGroup, darwinEmptyGroupPlist, &fakeExit{code: 181, stderr: "No such key: GroupMembership"})
		g, ok, err := NewAccounts("darwin", r.run).LookupGroup(ctx, "chainmail-clients")
		if err != nil || !ok {
			t.Fatalf("LookupGroup = %v, %v", ok, err)
		}
		if g.GID != 302 || len(g.Members) != 0 {
			t.Fatalf("group = %+v", g)
		}
	})

	t.Run("LookupGroup missing", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on(dsReadGroup, "", &fakeExit{code: 56, stderr: dsNotFound})
		_, ok, err := NewAccounts("darwin", r.run).LookupGroup(ctx, "chainmail-clients")
		if err != nil || ok {
			t.Fatalf("LookupGroup = %v, %v, want false, nil", ok, err)
		}
	})

	t.Run("CreateGroup", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on("dscl . -list /Groups PrimaryGroupID",
			"_amavisd                 83\nnobody                   -2\nfoo                      300\nbar                      301\nbaz                      303\n", nil)
		g, err := NewAccounts("darwin", r.run).CreateGroup(ctx, "chainmail-clients")
		if err != nil {
			t.Fatalf("CreateGroup: %v", err)
		}
		if g.Name != "chainmail-clients" || g.GID != 302 {
			t.Fatalf("group = %+v, want gid 302", g)
		}
		r.wantCalls(t,
			"dscl . -list /Groups PrimaryGroupID",
			"dseditgroup -o create -i 302 -r chainmail-clients chainmail-clients",
		)
	})

	t.Run("CreateGroup no free id", func(t *testing.T) {
		t.Parallel()
		var b strings.Builder
		for id := 300; id < 500; id++ {
			b.WriteString("g ")
			b.WriteString(strconv.Itoa(id))
			b.WriteString("\n")
		}
		r := newFakeRunner().on("dscl . -list /Groups PrimaryGroupID", b.String(), nil)
		if _, err := NewAccounts("darwin", r.run).CreateGroup(ctx, "chainmail-clients"); err == nil {
			t.Fatal("CreateGroup succeeded with no free gid")
		}
		r.wantCalls(t, "dscl . -list /Groups PrimaryGroupID")
	})

	t.Run("CreateDaemonUser", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().
			on(dsReadGroup, darwinGroupPlist, nil).
			on("dscl . -list /Users UniqueID", "root 0\nnobody -2\n_x 300\n", nil)
		u, err := NewAccounts("darwin", r.run).CreateDaemonUser(ctx, "_chainmail", "chainmail-clients", "/var/empty")
		if err != nil {
			t.Fatalf("CreateDaemonUser: %v", err)
		}
		want := User{Name: "_chainmail", UID: 301, GID: 302, Home: "/var/empty", Shell: "/usr/bin/false"}
		if !reflect.DeepEqual(u, want) {
			t.Fatalf("user = %+v, want %+v", u, want)
		}
		r.wantCalls(t,
			dsReadGroup,
			"dscl . -list /Users UniqueID",
			"dscl . -create /Users/_chainmail",
			"dscl . -create /Users/_chainmail UniqueID 301",
			"dscl . -create /Users/_chainmail PrimaryGroupID 302",
			"dscl . -create /Users/_chainmail UserShell /usr/bin/false",
			"dscl . -create /Users/_chainmail NFSHomeDirectory /var/empty",
			"dscl . -create /Users/_chainmail IsHidden 1",
		)
	})

	t.Run("CreateDaemonUser without the group", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on(dsReadGroup, "", &fakeExit{code: 56, stderr: dsNotFound})
		if _, err := NewAccounts("darwin", r.run).CreateDaemonUser(ctx, "_chainmail", "chainmail-clients", "/var/empty"); err == nil {
			t.Fatal("CreateDaemonUser succeeded without the clients group")
		}
		r.wantCalls(t, dsReadGroup)
	})

	t.Run("CreateDaemonUser failure stops", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().
			on(dsReadGroup, darwinGroupPlist, nil).
			on("dscl . -list /Users UniqueID", "root 0\n", nil).
			on("dscl . -create /Users/_chainmail UniqueID 300", "", &fakeExit{code: 1, stderr: "denied"})
		if _, err := NewAccounts("darwin", r.run).CreateDaemonUser(ctx, "_chainmail", "chainmail-clients", "/var/empty"); err == nil {
			t.Fatal("CreateDaemonUser succeeded when dscl failed")
		}
		r.wantCalls(t,
			dsReadGroup,
			"dscl . -list /Users UniqueID",
			"dscl . -create /Users/_chainmail",
			"dscl . -create /Users/_chainmail UniqueID 300",
		)
	})

	t.Run("AddMember", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner()
		if err := NewAccounts("darwin", r.run).AddMember(ctx, "chainmail-clients", "alice"); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
		r.wantCalls(t, "dseditgroup -o edit -a alice -t user chainmail-clients")
	})
}

func TestAccountsUnsupported(t *testing.T) {
	t.Parallel()
	r := newFakeRunner()
	a := NewAccounts("plan9", r.run)
	if _, _, err := a.LookupUser(context.Background(), "x"); !errors.Is(err, errUnsupported) {
		t.Fatalf("LookupUser error = %v, want errUnsupported", err)
	}
	r.wantCalls(t)
}

func TestParseDSPlist(t *testing.T) {
	t.Parallel()
	got, err := parseDSPlist([]byte(darwinGroupPlist))
	if err != nil {
		t.Fatalf("parseDSPlist: %v", err)
	}
	want := map[string][]string{
		"GroupMembership": {"alice", "bob"},
		"PrimaryGroupID":  {"302"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseDSPlist = %v, want %v", got, want)
	}
	if _, err := parseDSPlist([]byte("not xml <")); err == nil {
		t.Fatal("parseDSPlist accepted garbage")
	}
}
