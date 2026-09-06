package service

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeInfo is the minimum os.FileInfo a stat stub needs to return.
type fakeInfo struct{ name string }

func (f fakeInfo) Name() string       { return f.name }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return 0o644 }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return nil }

// stubStat makes osStat report exactly the named paths as existing.
func stubStat(t *testing.T, present ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, p := range present {
		set[p] = true
	}
	orig := osStat
	osStat = func(name string) (os.FileInfo, error) {
		if set[name] {
			return fakeInfo{name: name}, nil
		}
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { osStat = orig })
}

// THE BUG, PINNED: a WSL host has a SYSTEM unit and this package knew only about
// the user one.
//
// `dejima service install` writes ~/.config/systemd/user/dejimad.service.
// `dejima wsl setup` writes /etc/systemd/system/dejimad.service, because the WSL
// daemon runs as root and must start with the distro's init. Asking only about
// the user path meant: UnitInstalled false, Restart runs `systemctl --user
// restart` and fails, and the operator is told "no dejimad service is installed
// here" while the system unit is running the daemon they are talking to.
//
// #408 replaced "does systemd run?" with "is there a unit?" and implemented "a
// unit" as "a USER unit". The matrix gained a row and still did not have them all.
func TestSystemUnitCounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	userUnit := filepath.Join(home, ".config", "systemd", "user", systemdUnitName)

	t.Run("system unit only (the WSL shape)", func(t *testing.T) {
		stubStat(t, systemSystemdUnitPath)
		if !UnitInstalled() {
			t.Error("a system unit does not count as installed — this is the WSL bug")
		}
		if got := installedUnitScope(); got != unitSystem {
			t.Errorf("scope = %v, want unitSystem", got)
		}
		if !SystemUnit() {
			t.Error("SystemUnit() false with only a system unit present")
		}
	})

	t.Run("user unit only", func(t *testing.T) {
		stubStat(t, userUnit)
		if !UnitInstalled() {
			t.Error("a user unit must still count")
		}
		if SystemUnit() {
			t.Error("SystemUnit() true with only a user unit — the hint would name the wrong scope")
		}
	})

	t.Run("neither", func(t *testing.T) {
		stubStat(t)
		if UnitInstalled() {
			t.Error("no unit anywhere reported as installed")
		}
		if SystemUnit() {
			t.Error("SystemUnit() true with no unit at all")
		}
	})

	t.Run("both — user wins so existing hosts are unchanged", func(t *testing.T) {
		stubStat(t, userUnit, systemSystemdUnitPath)
		if !UnitInstalled() {
			t.Error("both present but not installed")
		}
		if SystemUnit() {
			t.Error("a host with both must keep using its user unit")
		}
	})
}

// The scope decides the COMMAND, and naming the wrong one is not cosmetic: a
// system unit restarted with --user fails saying the unit does not exist.
func TestSystemctlArgsMatchTheScope(t *testing.T) {
	if got := strings.Join(systemctlArgs(unitUser, "restart", systemdUnitName), " "); got != "--user restart dejimad.service" {
		t.Errorf("user scope = %q", got)
	}
	if got := strings.Join(systemctlArgs(unitSystem, "restart", systemdUnitName), " "); got != "restart dejimad.service" {
		t.Errorf("system scope = %q — --user against a system unit fails as 'unit not found'", got)
	}
}

// A restart with no unit anywhere must say so rather than shelling out and
// returning systemctl's error, which names a scope the operator never chose.
func TestRestartWithNoUnitNamesBothScopes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubStat(t)
	err := (&systemdManager{}).Restart()
	if err == nil {
		t.Fatal("restart with no unit returned nil")
	}
	for _, want := range []string{systemSystemdUnitPath, "dejima service install", "dejima wsl setup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}
