package service

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/aoos/dejima/internal/fdlimit"
)

const systemdUnitName = "dejimad.service"

type systemdManager struct{}

func (m *systemdManager) unitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", systemdUnitName), nil
}

// systemSystemdUnitPath is where `dejima wsl setup` writes the unit INSIDE a WSL
// distro. It is a SYSTEM unit, not a user unit, because the WSL daemon runs as
// root with HOME=/root and has to start with the distro's own init — see
// cmd/dejima/wslservice.go.
const systemSystemdUnitPath = "/etc/systemd/system/" + systemdUnitName

// unitScope names which systemd scope, if any, actually holds a dejimad unit.
type unitScope int

const (
	unitNone   unitScope = iota
	unitUser             // ~/.config/systemd/user  — `dejima service install`
	unitSystem           // /etc/systemd/system     — `dejima wsl setup`
)

// osStat is a seam so the scope logic is testable without a systemd host.
var osStat = os.Stat

// installedUnitScope reports which scope holds a dejimad unit.
//
// BOTH are real and this package used to know only one. `dejima service install`
// writes a USER unit; `dejima wsl setup` writes a SYSTEM unit at
// /etc/systemd/system, because the WSL daemon runs as root and must start with
// the distro's init. Everything here asked only about the user unit, so on a WSL
// host the answers were: UnitInstalled false, Restart runs `systemctl --user
// restart` and fails, and the operator is told "no dejimad service is installed
// here" while /etc/systemd/system/dejimad.service is sitting there running the
// daemon they are talking to.
//
// That is #408's bug one layer down. #408 replaced "does systemd run?" with "is
// there a unit?" — correct, and then implemented "a unit" as "a USER unit". The
// matrix gained a row and still did not have them all.
//
// User is checked FIRST so a host with both keeps its existing behaviour.
func installedUnitScope() unitScope {
	m := &systemdManager{}
	if path, err := m.unitPath(); err == nil {
		if _, statErr := osStat(path); statErr == nil {
			return unitUser
		}
	}
	if _, err := osStat(systemSystemdUnitPath); err == nil {
		return unitSystem
	}
	return unitNone
}

// UnitInstalled reports whether a dejimad unit exists on this host, in EITHER
// scope.
//
// Distinct from "does this host run systemd", and the distinction is the whole
// point: a WSL distro can run systemd perfectly well and still have no dejimad
// unit, because the daemon there may be launched from the Windows side. An
// operator in exactly that state was told to run `sudo systemctl restart
// dejimad` after a self-update, and the restart the daemon had just attempted
// had already failed with "is the service installed?".
func UnitInstalled() bool {
	return installedUnitScope() != unitNone
}

// systemctlArgs prefixes `--user` only when the unit is a user unit. A system
// unit restarted with --user fails with a message about the unit not existing,
// which is how a WSL self-update installed its binary and then reported that
// nothing was installed.
func systemctlArgs(scope unitScope, args ...string) []string {
	if scope == unitUser {
		return append([]string{"--user"}, args...)
	}
	return args
}

func (m *systemdManager) Install(binaryPath string, args []string) error {
	path, err := m.unitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	execStart := binaryPath
	if len(args) > 0 {
		execStart += " " + strings.Join(args, " ")
	}
	tmpl := template.Must(template.New("unit").Parse(systemdTemplate))
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{
		"ExecStart":   execStart,
		"LimitNOFILE": fdlimit.Target,
	}); err != nil {
		return err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return err
	}
	if err := exec.Command("systemctl", "--user", "daemon-reload").Run(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if err := exec.Command("systemctl", "--user", "enable", "--now", systemdUnitName).Run(); err != nil {
		return fmt.Errorf("systemctl enable: %w", err)
	}
	return nil
}

func (m *systemdManager) Uninstall() error {
	_ = exec.Command("systemctl", "--user", "disable", "--now", systemdUnitName).Run()
	path, err := m.unitPath()
	if err != nil {
		return err
	}
	_ = os.Remove(path)
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	return nil
}

func (m *systemdManager) Restart() error {
	scope := installedUnitScope()
	if scope == unitNone {
		return fmt.Errorf("no %s exists in either systemd scope (%s or the user unit), "+
			"so there is nothing to restart — install it with `dejima service install`, "+
			"or in WSL with `dejima wsl setup`", systemdUnitName, systemSystemdUnitPath)
	}
	args := systemctlArgs(scope, "restart", systemdUnitName)
	// The message names EXACTLY what ran. It used to say plain `systemctl restart
	// dejimad.service` while running the --user form, so an operator copying the
	// error out of the log ran a different command and got a different failure
	// than the one being reported to them.
	if err := exec.Command("systemctl", args...).Run(); err != nil {
		return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func (m *systemdManager) Status() (string, error) {
	args := systemctlArgs(installedUnitScope(), "is-active", systemdUnitName)
	out, _ := exec.Command("systemctl", args...).Output()
	return strings.TrimSpace(string(out)), nil
}

const systemdTemplate = `[Unit]
Description=Dejima host daemon
After=network.target docker.service

[Service]
ExecStart={{.ExecStart}}
Restart=on-failure
RestartSec=5
# Each island egress tunnel costs two descriptors and lives as long as the
# agent's session; the distro default is not sized for that. The daemon also
# raises this itself at startup.
LimitNOFILE={{.LimitNOFILE}}

[Install]
WantedBy=default.target
`

// SystemUnit reports whether the installed dejimad unit is a SYSTEM unit rather
// than a user unit — which is the WSL shape, since `dejima wsl setup` writes to
// /etc/systemd/system so the daemon starts with the distro's init as root.
//
// Exported because a caller outside this package composes the restart advice an
// operator will copy and run, and advice naming the wrong scope fails with a
// message about a unit that "does not exist" while the unit sits on disk.
func SystemUnit() bool { return installedUnitScope() == unitSystem }
