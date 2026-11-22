package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/stubbedev/phix/internal/supervisor"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "service" {
		os.Exit(Main(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func TestServiceRefusesOnNixOS(t *testing.T) {
	h := newHarness(t)
	_, errOut, code := h.run("", "service")
	if code != 1 || !strings.Contains(errOut, "systemctl --user") {
		t.Fatalf("phix service on NixOS: exit %d, stderr %q", code, errOut)
	}
}

func TestWriteChangesMatchesSwitchToConfiguration(t *testing.T) {
	var out bytes.Buffer
	writeChanges(&out, []supervisor.Change{
		{Action: supervisor.ActionStart, Unit: "new.service"},
		{Action: supervisor.ActionStop, Unit: "gone.service"},
		{Action: supervisor.ActionRestart, Unit: "a.service"},
		{Action: supervisor.ActionRestart, Unit: "b.timer"},
	})
	want := "stopping the following user units: gone.service\n" +
		"restarting the following user units: a.service, b.timer\n" +
		"starting the following user units: new.service\n"
	if out.String() != want {
		t.Errorf("writeChanges:\n%s\nwant:\n%s", out.String(), want)
	}
}

func androidHarness(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	cfg, err := yaml.Marshal(map[string]any{
		"flake":    h.flake,
		"platform": "android",
		"attr":     "phone",
		"ssh_key":  filepath.Join(h.home, ".ssh", "id_ed25519"),
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(h.dir, "android.yaml")
	if err := os.WriteFile(cfgPath, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PHIX_CONFIG", cfgPath)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h.home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(h.home, ".local", "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(h.home, ".local", "share"))
	runtime, err := os.MkdirTemp("", "phx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runtime) })
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	units := filepath.Join(h.home, ".config", "systemd", "user")
	if err := os.MkdirAll(filepath.Join(units, "default.target.wants"), 0o755); err != nil {
		t.Fatal(err)
	}
	return h, units
}

func writeUnit(t *testing.T, units, name, content string) {
	t.Helper()
	src := filepath.Join(units, "..", "store-"+name)
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{filepath.Join(units, name), filepath.Join(units, "default.target.wants", name)} {
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.Symlink(src, link); err != nil {
			t.Fatal(err)
		}
	}
}

func TestServiceLifecycle(t *testing.T) {
	h, units := androidHarness(t)
	writeUnit(t, units, "app.service", "[Unit]\nDescription=Demo\n[Service]\nExecStart=/bin/sh -c 'echo hello; exec sleep 3600'\nProtectHome=yes\n")
	t.Cleanup(func() { h.run("", "service", "down") })

	out := h.mustRun("", "service", "sync")
	if out != "starting the following user units: app.service\n" {
		t.Errorf("first sync = %q, want it to boot the supervisor and report app.service", out)
	}
	if _, errOut, code := h.run("", "service", "up", "-v"); code != 0 || !strings.Contains(errOut, "already running") {
		t.Errorf("service up: exit %d, stderr %q", code, errOut)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(h.mustRun("", "service", "logs", "app"), "hello") {
		if time.Now().After(deadline) {
			t.Fatalf("app never logged its greeting: %q", h.mustRun("", "service", "logs", "app"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	table := h.mustRun("", "service")
	if !strings.Contains(table, "app.service") || !strings.Contains(table, "active (running)") {
		t.Errorf("status table:\n%s", table)
	}
	detail := h.mustRun("", "service", "status", "app")
	for _, want := range []string{"app.service - Demo", "Ignored: Service.ProtectHome", "Main PID:", "hello"} {
		if !strings.Contains(detail, want) {
			t.Errorf("status app lacks %q:\n%s", want, detail)
		}
	}

	writeUnit(t, units, "app.service", "[Service]\nExecStart=/bin/sh -c 'echo again; exec sleep 3600'\n")
	if out := h.mustRun("", "service", "sync"); out != "restarting the following user units: app.service\n" {
		t.Errorf("sync after a change = %q", out)
	}
	if out := h.mustRun("", "service", "sync"); out != "" {
		t.Errorf("sync without changes = %q, want nothing", out)
	}
	h.mustRun("", "service", "stop", "app")
	if !strings.Contains(h.mustRun("", "service", "status", "app"), "inactive (dead)") {
		t.Error("app is not inactive after stop")
	}
	if _, errOut, code := h.run("", "service", "start", "missing"); code != 1 || !strings.Contains(errOut, "missing.service") {
		t.Errorf("starting a missing unit: exit %d, stderr %q", code, errOut)
	}
	h.mustRun("", "service", "down")
	if !strings.Contains(h.mustRun("", "service", "logs"), "stopped") {
		t.Error("the supervisor log is unreadable or lacks its shutdown once the supervisor is down")
	}
	if _, errOut, code := h.run("", "service"); code != 1 || !strings.Contains(errOut, "not running") {
		t.Errorf("status after down: exit %d, stderr %q", code, errOut)
	}
}

func TestSystemctlAndJournalctlShims(t *testing.T) {
	h, units := androidHarness(t)
	writeUnit(t, units, "app.service", "[Service]\nExecStart=/bin/sh -c 'echo hello; exec sleep 3600'\n")
	t.Cleanup(func() { h.run("", "service", "down") })
	if out, _, code := h.run("", "service", "systemctl", "--user", "is-system-running"); code != 1 || out != "offline\n" {
		t.Errorf("is-system-running without a supervisor: exit %d, %q", code, out)
	}
	h.mustRun("", "service", "sync")
	deadline := time.Now().Add(10 * time.Second)
	for {
		out := h.mustRun("", "service", "journalctl", "--user", "-u", "app", "-o", "cat")
		if slices.Contains(strings.Split(out, "\n"), "hello") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("journalctl -o cat never showed the bare line: %q", out)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if out := h.mustRun("", "service", "systemctl", "--user", "is-active", "app"); out != "active\n" {
		t.Errorf("is-active = %q", out)
	}
	if out := h.mustRun("", "service", "systemctl", "--user", "is-enabled", "app.service"); out != "enabled\n" {
		t.Errorf("is-enabled = %q", out)
	}
	if out := h.mustRun("", "service", "systemctl", "--user", "is-system-running"); out != "running\n" {
		t.Errorf("is-system-running = %q", out)
	}
	if out := h.mustRun("", "service", "systemctl", "--user", "cat", "app"); !strings.Contains(out, "ExecStart=/bin/sh") {
		t.Errorf("cat = %q", out)
	}
	h.mustRun("", "service", "systemctl", "--user", "stop", "app")
	if out, _, code := h.run("", "service", "systemctl", "--user", "is-active", "app"); code != 1 || out != "inactive\n" {
		t.Errorf("is-active after stop: exit %d, %q", code, out)
	}
	h.mustRun("", "service", "systemctl", "--user", "try-restart", "app")
	if out, _, _ := h.run("", "service", "systemctl", "--user", "is-active", "app"); out != "inactive\n" {
		t.Errorf("try-restart started an inactive unit: %q", out)
	}
	h.mustRun("", "service", "systemctl", "--user", "daemon-reload")
	if _, errOut, code := h.run("", "service", "systemctl", "--user", "enable", "--now", "app"); code != 1 || !strings.Contains(errOut, "home-manager") {
		t.Errorf("enable: exit %d, %q", code, errOut)
	}
	if _, _, code := h.run("", "service", "systemctl", "start", "app"); code != 2 {
		t.Errorf("systemctl without --user exited %d, want 2", code)
	}
}

func TestServiceExecRejectsOutOfRangeUMask(t *testing.T) {
	h := newHarness(t)
	for _, mask := range []string{"1000", "40000000000", "9", "-1"} {
		_, errOut, code := h.run("", "service", "exec", "--umask", mask, "--", "/bin/true", "true")
		if code != 2 || !strings.Contains(errOut, `"`+mask+`"`) {
			t.Errorf("service exec --umask %s: exit %d, stderr %q; want exit 2 naming the mask", mask, code, errOut)
		}
	}
}
