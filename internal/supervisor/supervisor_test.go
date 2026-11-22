package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stubbedev/phix/internal/unit"
)

const helperEnv = "PHIX_SUPERVISOR_TEST_HELPER"

func TestMain(m *testing.M) {
	if len(os.Args) > 3 && os.Args[1] == "service" && os.Args[2] == "exec" {
		execHelper(os.Args[3:])
		return
	}
	switch os.Getenv(helperEnv) {
	case "notify":
		notifyHelper()
		return
	case "socket":
		socketHelper()
		return
	case "attributes":
		attributesHelper()
		return
	}
	os.Exit(m.Run())
}

func notifyHelper() {
	conn, err := net.Dial("unixgram", os.Getenv("NOTIFY_SOCKET"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	time.Sleep(200 * time.Millisecond)
	fmt.Fprint(conn, "STATUS=warming up\nREADY=1")
	conn.Close()
	time.Sleep(time.Hour)
}

func attributesHelper() {
	prio, err := syscall.Getpriority(syscall.PRIO_PROCESS, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(9)
	}
	if runtime.GOOS == "linux" {
		prio = 20 - prio
	}
	mask := syscall.Umask(0)
	fmt.Printf("nice=%d umask=%04o\n", prio, mask)
	if err := os.WriteFile(os.Getenv("ATTRIBUTES_OUT"), fmt.Appendf(nil, "%d %04o", prio, mask), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(10)
	}
}

func socketHelper() {
	if os.Getenv("LISTEN_PID") != strconv.Itoa(os.Getpid()) || os.Getenv("LISTEN_FDS") != "1" {
		fmt.Fprintf(os.Stderr, "LISTEN_PID=%s LISTEN_FDS=%s pid=%d\n", os.Getenv("LISTEN_PID"), os.Getenv("LISTEN_FDS"), os.Getpid())
		os.Exit(6)
	}
	l, err := net.FileListener(os.NewFile(3, "listen"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(7)
	}
	for {
		conn, err := l.Accept()
		if err != nil {
			os.Exit(8)
		}
		fmt.Fprintf(conn, "hello %s\n", os.Getenv("LISTEN_FDNAMES"))
		conn.Close()
	}
}

func execHelper(args []string) {
	var spec ExecSpec
	for len(args) > 0 && args[0] != "--" {
		if args[0] == "--listen-pid" {
			spec.ListenPID = true
			args = args[1:]
			continue
		}
		if args[0] == "--umask" {
			mask, err := unit.ParseUMask(args[1])
			if err != nil {
				os.Exit(4)
			}
			spec.UMask = &mask
			args = args[2:]
			continue
		}
		v, err := strconv.Atoi(args[1])
		if err != nil {
			os.Exit(4)
		}
		switch args[0] {
		case "--nice":
			spec.Nice = &v
		case "--io-class":
			spec.IOClass = &v
		case "--io-priority":
			spec.IOPriority = &v
		}
		args = args[2:]
	}
	if err := Exec(spec, args[1], args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(5)
	}
}

type harness struct {
	t      *testing.T
	layout Layout
	client Client
	store  string
	cancel context.CancelFunc
	done   chan error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root, err := os.MkdirTemp("", "phx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	home := filepath.Join(root, "h")
	host := unit.Host{
		Home:       home,
		User:       "tester",
		UID:        os.Getuid(),
		GID:        os.Getgid(),
		Hostname:   "phone",
		ConfigHome: filepath.Join(home, ".config"),
		DataHome:   filepath.Join(home, ".local", "share"),
		StateHome:  filepath.Join(home, ".local", "state"),
		CacheHome:  filepath.Join(home, ".cache"),
		RuntimeDir: filepath.Join(root, "r"),
		TempDir:    filepath.Join(root, "t"),
	}
	for _, d := range []string{home, host.UnitDirs()[0], host.TempDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	layout := Layout{Host: host, State: filepath.Join(host.StateHome, "phix", "services"), Runtime: filepath.Join(host.RuntimeDir, "phix")}
	return &harness{t: t, layout: layout, client: NewClient(layout), store: filepath.Join(root, "store")}
}

func (h *harness) install(name, content string, wantedBy ...string) {
	h.t.Helper()
	src := filepath.Join(h.store, strconv.FormatInt(time.Now().UnixNano(), 36), name)
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
	dir := h.layout.Host.UnitDirs()[0]
	h.relink(filepath.Join(dir, name), src)
	for _, w := range wantedBy {
		wdir := filepath.Join(dir, w+".wants")
		if err := os.MkdirAll(wdir, 0o755); err != nil {
			h.t.Fatal(err)
		}
		h.relink(filepath.Join(wdir, name), src)
	}
}

func (h *harness) relink(link, target string) {
	h.t.Helper()
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		h.t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) uninstall(name string) {
	h.t.Helper()
	dir := h.layout.Host.UnitDirs()[0]
	matches, err := filepath.Glob(filepath.Join(dir, "*.wants", name))
	if err != nil {
		h.t.Fatal(err)
	}
	for _, p := range append(matches, filepath.Join(dir, name)) {
		if err := os.Remove(p); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) start() {
	h.t.Helper()
	self, err := os.Executable()
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	readyCh := make(chan error, 1)
	go func() {
		h.done <- Supervise(ctx, h.layout, Program{Executable: self}, os.Environ(), func(err error) { readyCh <- err })
	}()
	if err := <-readyCh; err != nil {
		h.t.Fatalf("supervisor did not become ready: %v", err)
	}
	h.t.Cleanup(h.stop)
}

func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("supervisor returned %v", err)
		}
	case <-time.After(30 * time.Second):
		h.t.Error("supervisor did not shut down within 30s")
	}
	h.cancel = nil
}

func (h *harness) status(name string) UnitStatus {
	h.t.Helper()
	resp, err := h.client.Call(context.Background(), Request{Op: OpStatus, Units: []string{name}})
	if err != nil {
		h.t.Fatalf("status %s: %v", name, err)
	}
	return resp.Status[0]
}

func (h *harness) waitFor(name, what string, ok func(UnitStatus) bool) UnitStatus {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		s := h.status(name)
		if ok(s) {
			return s
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%s never became %s; last status %+v; log:\n%s", name, what, s, strings.Join(h.logs(name), "\n"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *harness) waitState(name, active, sub string) UnitStatus {
	h.t.Helper()
	return h.waitFor(name, active+"/"+sub, func(s UnitStatus) bool { return s.Active == active && (sub == "" || s.Sub == sub) })
}

func (h *harness) logs(name string) []string {
	h.t.Helper()
	logName, err := LogName([]string{name})
	if err != nil {
		h.t.Fatal(err)
	}
	lines, err := ReadLog(h.layout, logName, -1)
	if err != nil {
		h.t.Fatalf("logs %s: %v", name, err)
	}
	return lines
}

func (h *harness) control(op Op, names ...string) Response {
	h.t.Helper()
	resp, err := h.client.Call(context.Background(), Request{Op: op, Units: names})
	if err != nil {
		h.t.Fatalf("%s %v: %v", op, names, err)
	}
	return resp
}

func (h *harness) file(name string) string { return filepath.Join(h.layout.Host.Home, name) }

func (h *harness) waitFile(name string, check func(string) bool) string {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		data, _ := os.ReadFile(h.file(name))
		if check(string(data)) {
			return string(data)
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%s never satisfied the check; content %q", name, data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func containsLine(lines []string, fragment string) bool {
	return slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, fragment) })
}

func TestBootOrderingAndLogs(t *testing.T) {
	h := newHarness(t)
	h.install("prepare.service", `[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'sleep 0.3; echo prepared > %h/prepared'
`, "default.target")
	h.install("app.service", `[Unit]
Description=The app
After=prepare.service
Requires=prepare.service
[Service]
Environment="GREETING=hello world"
ExecStart=/bin/sh -c 'cat %h/prepared; echo "$GREETING from $XDG_RUNTIME_DIR"; exec sleep 3600'
`, "default.target")
	h.start()
	s := h.waitState("app.service", "active", "running")
	if s.MainPID == 0 || s.Description != "The app" {
		t.Errorf("app status = %+v", s)
	}
	h.waitState("prepare.service", "active", "exited")
	deadline := time.Now().Add(5 * time.Second)
	for !containsLine(h.logs("app"), "hello world from "+h.layout.Host.RuntimeDir) {
		if time.Now().After(deadline) {
			t.Fatalf("app log never showed its output: %q", h.logs("app"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !containsLine(h.logs("app"), "prepared") {
		t.Errorf("app started before prepare.service finished: %q", h.logs("app"))
	}
}

func TestRestartPolicyAndStartLimit(t *testing.T) {
	h := newHarness(t)
	h.install("flaky.service", `[Unit]
StartLimitIntervalSec=10s
StartLimitBurst=3
[Service]
ExecStart=/bin/sh -c 'echo run >> %h/runs; exit 3'
Restart=on-failure
RestartSec=50ms
`, "default.target")
	h.start()
	s := h.waitState("flaky.service", "failed", "failed")
	if s.Result != "start-limit-hit" {
		t.Errorf("result = %q, want start-limit-hit", s.Result)
	}
	data, err := os.ReadFile(h.file("runs"))
	if err != nil {
		t.Fatal(err)
	}
	if runs := strings.Count(string(data), "run"); runs != 3 {
		t.Errorf("ran %d times, want 3 (the burst)", runs)
	}
}

func TestCleanExitWithoutRestart(t *testing.T) {
	h := newHarness(t)
	h.install("once.service", "[Service]\nExecStart=true\nRestart=on-failure\n", "default.target")
	h.install("broken.service", "[Service]\nExecStart=/bin/sh -c 'exit 7'\n", "default.target")
	h.start()
	if s := h.waitState("once.service", "inactive", "dead"); s.Result != "success" {
		t.Errorf("once result = %q", s.Result)
	}
	if s := h.waitState("broken.service", "failed", "failed"); s.Result != "exit-code" {
		t.Errorf("broken result = %q", s.Result)
	}
}

func TestStopRunsExecStopAndKillsTheGroup(t *testing.T) {
	h := newHarness(t)
	h.install("daemon.service", `[Service]
ExecStart=/bin/sh -c 'sleep 3600 & echo $! > %h/child; wait'
ExecStop=/bin/sh -c 'echo $MAINPID > %h/stopped'
TimeoutStopSec=5s
`, "default.target")
	h.start()
	s := h.waitState("daemon.service", "active", "running")
	child := strings.TrimSpace(h.waitFile("child", func(c string) bool { return strings.TrimSpace(c) != "" }))
	resp := h.control(OpStop, "daemon")
	if len(resp.Failures) > 0 {
		t.Fatalf("stop failed: %v", resp.Failures)
	}
	h.waitState("daemon.service", "inactive", "dead")
	stopped, err := os.ReadFile(h.file("stopped"))
	if err != nil || strings.TrimSpace(string(stopped)) != strconv.Itoa(s.MainPID) {
		t.Errorf("ExecStop saw MAINPID %q (%v), want %d", stopped, err, s.MainPID)
	}
	pid, err := strconv.Atoi(child)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Errorf("the forked child %d survived the stop", pid)
	}
}

func TestStopEscalatesToSIGKILL(t *testing.T) {
	h := newHarness(t)
	h.install("stubborn.service", `[Service]
ExecStart=/bin/sh -c 'trap "" TERM; echo ok > %h/trapped; while :; do sleep 0.05; done'
TimeoutStopSec=300ms
`, "default.target")
	h.start()
	h.waitState("stubborn.service", "active", "running")
	h.waitFile("trapped", func(c string) bool { return c != "" })
	h.control(OpStop, "stubborn.service")
	if s := h.waitState("stubborn.service", "failed", "failed"); s.Result != "timeout" {
		t.Errorf("result = %q, want timeout", s.Result)
	}
}

func TestNotifyReadiness(t *testing.T) {
	h := newHarness(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h.install("notifier.service", fmt.Sprintf(`[Service]
Type=notify
Environment=%s=notify
ExecStart=%s
ExecStartPost=/bin/sh -c 'touch %%h/post'
`, helperEnv, self), "default.target")
	h.start()
	s := h.waitState("notifier.service", "active", "running")
	if s.StatusText != "warming up" {
		t.Errorf("status text = %q", s.StatusText)
	}
	if _, err := os.Stat(h.file("post")); err != nil {
		t.Errorf("ExecStartPost did not run before the unit became active: %v", err)
	}
}

func TestNotifyFailsWhenMainExitsBeforeReady(t *testing.T) {
	h := newHarness(t)
	h.install("liar.service", "[Service]\nType=notify\nExecStart=/bin/sh -c 'exit 0'\n", "default.target")
	h.start()
	h.waitState("liar.service", "failed", "failed")
}

func TestTimerTriggersService(t *testing.T) {
	h := newHarness(t)
	h.install("tick.timer", "[Timer]\nOnActiveSec=100ms\nOnUnitActiveSec=200ms\n", "timers.target")
	h.install("tick.service", "[Service]\nType=oneshot\nExecStart=/bin/sh -c 'echo tick >> %h/ticks'\n")
	h.start()
	h.waitFile("ticks", func(c string) bool { return strings.Count(c, "tick") >= 3 })
	s := h.status("tick.timer")
	if s.Active != "active" || s.Triggers != "tick.service" || s.LastTrigger.IsZero() {
		t.Errorf("timer status = %+v", s)
	}
}

func TestPersistentTimerCatchesUp(t *testing.T) {
	h := newHarness(t)
	h.install("weekly.timer", "[Timer]\nOnCalendar=weekly\nPersistent=true\n", "timers.target")
	h.install("weekly.service", "[Service]\nType=oneshot\nExecStart=/bin/sh -c 'echo ran >> %h/weekly'\n")
	if err := os.MkdirAll(h.layout.StampDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if err := os.WriteFile(filepath.Join(h.layout.StampDir(), "weekly.timer"), []byte(stamp+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.start()
	h.waitFile("weekly", func(c string) bool { return strings.Count(c, "ran") == 1 })
	s := h.waitFor("weekly.timer", "rescheduled", func(s UnitStatus) bool { return s.NextElapse.After(time.Now()) })
	if s.NextElapse.Weekday() != time.Monday && s.NextElapse.In(time.Local).Weekday() != time.Monday {
		t.Errorf("next elapse %v is not a Monday", s.NextElapse)
	}
}

func TestSyncRestartsChangedStopsRemovedStartsNew(t *testing.T) {
	h := newHarness(t)
	h.install("keep.service", "[Service]\nExecStart=sleep 3600\n", "default.target")
	h.install("change.service", "[Service]\nExecStart=sleep 3600\n", "default.target")
	h.install("gone.service", "[Service]\nExecStart=sleep 3600\n", "default.target")
	h.install("old.service", "[Unit]\nX-SwitchMethod=keep-old\n[Service]\nExecStart=sleep 3600\n", "default.target")
	h.start()
	for _, n := range []string{"keep", "change", "gone", "old"} {
		h.waitState(n+".service", "active", "running")
	}
	keepPID := h.status("keep.service").MainPID
	changePID := h.status("change.service").MainPID
	oldPID := h.status("old.service").MainPID

	h.install("change.service", "[Service]\nExecStart=sleep 7200\n", "default.target")
	h.install("old.service", "[Unit]\nX-SwitchMethod=keep-old\n[Service]\nExecStart=sleep 7200\n", "default.target")
	h.uninstall("gone.service")
	h.install("new.service", "[Service]\nExecStart=sleep 3600\n", "default.target")

	resp := h.control(OpSync)
	want := []Change{
		{ActionRestart, "change.service"},
		{ActionStop, "gone.service"},
		{ActionStart, "new.service"},
	}
	if !slices.Equal(resp.Changes, want) || len(resp.Failures) > 0 {
		t.Fatalf("sync = %+v, want changes %+v", resp, want)
	}
	if s := h.status("change.service"); s.Active != "active" || s.MainPID == changePID {
		t.Errorf("change.service = %+v, want restarted", s)
	}
	if s := h.status("keep.service"); s.MainPID != keepPID {
		t.Errorf("keep.service was restarted")
	}
	if s := h.status("old.service"); s.MainPID != oldPID {
		t.Errorf("keep-old unit old.service was restarted")
	}
	if _, err := h.client.Call(context.Background(), Request{Op: OpStatus, Units: []string{"gone.service"}}); err == nil {
		t.Error("gone.service is still known after it was removed")
	}
	h.waitState("new.service", "active", "running")
}

func TestPartOfPropagatesRestart(t *testing.T) {
	h := newHarness(t)
	h.install("parent.service", "[Service]\nExecStart=sleep 3600\n", "default.target")
	h.install("child.service", "[Unit]\nPartOf=parent.service\nAfter=parent.service\n[Service]\nExecStart=sleep 3600\n", "default.target")
	h.start()
	h.waitState("child.service", "active", "running")
	before := h.status("child.service").MainPID
	h.control(OpRestart, "parent")
	h.waitState("child.service", "active", "running")
	if after := h.status("child.service").MainPID; after == before {
		t.Error("restarting parent.service did not restart its PartOf child")
	}
	h.control(OpStop, "parent")
	h.waitState("child.service", "inactive", "dead")
}

func TestProcessAttributesThroughTheHelper(t *testing.T) {
	h := newHarness(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h.install("nice.service", fmt.Sprintf("[Service]\nType=oneshot\nNice=7\nUMask=0077\nEnvironment=%s=attributes ATTRIBUTES_OUT=%%h/attributes\nExecStart=%s\n", helperEnv, self), "default.target")
	h.start()
	h.waitState("nice.service", "inactive", "dead")
	if got := h.waitFile("attributes", func(c string) bool { return c != "" }); got != "7 0077" {
		t.Errorf("nice and umask = %q, want \"7 0077\"", got)
	}
}

func TestConditionSkipsWithoutFailingDependents(t *testing.T) {
	h := newHarness(t)
	h.install("cond.service", "[Unit]\nConditionPathExists=/nonexistent/phix\n[Service]\nExecStart=sleep 3600\n", "default.target")
	h.install("after.service", "[Unit]\nRequires=cond.service\nAfter=cond.service\n[Service]\nExecStart=sleep 3600\n", "default.target")
	h.start()
	h.waitState("after.service", "active", "running")
	if s := h.status("cond.service"); s.Active != "inactive" || s.Result != "condition" {
		t.Errorf("cond.service = %+v, want skipped by its condition", s)
	}
}

func TestRequiresFailurePropagates(t *testing.T) {
	h := newHarness(t)
	h.install("base.service", "[Service]\nType=oneshot\nExecStart=false\n", "default.target")
	h.install("top.service", "[Unit]\nRequires=base.service\nAfter=base.service\n[Service]\nExecStart=sleep 3600\n", "default.target")
	h.start()
	h.waitState("base.service", "failed", "failed")
	resp := h.control(OpStart, "top.service")
	if len(resp.Failures) != 1 || !strings.Contains(resp.Failures[0], "base.service") {
		t.Errorf("starting top.service = %+v, want a dependency failure", resp)
	}
	if s := h.status("top.service"); s.Active == "active" {
		t.Errorf("top.service started despite its failed requirement")
	}
}

func TestShutdownStopsEverything(t *testing.T) {
	h := newHarness(t)
	h.install("a.service", "[Service]\nExecStart=sleep 3600\nExecStopPost=/bin/sh -c 'touch %h/stopped'\n", "default.target")
	h.start()
	h.waitState("a.service", "active", "running")
	if _, err := h.client.Call(context.Background(), Request{Op: OpShutdown}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.file("stopped")); err != nil {
		t.Errorf("ExecStopPost did not run on shutdown: %v", err)
	}
	h.stop()
}

func TestSecondSupervisorReportsRunning(t *testing.T) {
	h := newHarness(t)
	h.start()
	err := Supervise(context.Background(), h.layout, Program{Executable: "/bin/false"}, nil, func(err error) {
		if !errors.Is(err, ErrAlreadyRunning) {
			t.Errorf("ready = %v, want ErrAlreadyRunning", err)
		}
	})
	if err != nil {
		t.Errorf("second Supervise = %v", err)
	}
}

func TestPlanSync(t *testing.T) {
	mk := func(digest string, method unit.SwitchMethod) *unit.Unit {
		return &unit.Unit{Digest: digest, SwitchMethod: method}
	}
	n := func(s string) unit.Name {
		v, err := unit.ParseName(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	def := func(wants ...string) *unit.Unit {
		u := &unit.Unit{Name: n("default.target"), Digest: "d"}
		for _, w := range wants {
			u.Deps.Wants = append(u.Deps.Wants, n(w))
		}
		return u
	}
	old := &unit.Set{Units: map[unit.Name]*unit.Unit{
		n("default.target"): def("a.service", "b.service", "r.service"),
		n("a.service"):      mk("1", unit.SwitchRestart),
		n("b.service"):      mk("1", unit.SwitchReload),
		n("r.service"):      mk("1", unit.SwitchRestart),
		n("idle.service"):   mk("1", unit.SwitchRestart),
		n("failed.service"): mk("1", unit.SwitchRestart),
	}, Errors: map[unit.Name]error{}}
	next := &unit.Set{Units: map[unit.Name]*unit.Unit{
		n("default.target"): def("a.service", "b.service", "idle.service", "failed.service", "new.service"),
		n("a.service"):      mk("2", unit.SwitchRestart),
		n("b.service"):      mk("2", unit.SwitchReload),
		n("idle.service"):   mk("1", unit.SwitchRestart),
		n("failed.service"): mk("2", unit.SwitchRestart),
		n("new.service"):    mk("1", unit.SwitchRestart),
	}, Errors: map[unit.Name]error{}}
	active := map[string]bool{"default.target": true, "a.service": true, "b.service": true, "r.service": true}
	got := planSync(old, next, func(x unit.Name) bool { return active[x.String()] })
	want := []Change{
		{ActionRestart, "a.service"},
		{ActionReload, "b.service"},
		{ActionStart, "failed.service"},
		{ActionStart, "idle.service"},
		{ActionStart, "new.service"},
		{ActionStop, "r.service"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("planSync = %+v\nwant %+v", got, want)
	}
}

func TestNextElapse(t *testing.T) {
	base := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	cal, err := unit.ParseCalendar("daily", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		timer unit.TimerConfig
		in    scheduleInputs
		want  time.Time
		ok    bool
	}{
		{"boot", unit.TimerConfig{OnBoot: []time.Duration{30 * time.Second}}, scheduleInputs{started: base, activated: base}, base.Add(30 * time.Second), true},
		{"boot already fired", unit.TimerConfig{OnBoot: []time.Duration{30 * time.Second}}, scheduleInputs{started: base, activated: base, lastTrigger: base.Add(31 * time.Second)}, time.Time{}, false},
		{"unit active after boot", unit.TimerConfig{OnBoot: []time.Duration{30 * time.Second}, OnUnitActive: []time.Duration{10 * time.Minute}}, scheduleInputs{started: base, activated: base, lastTrigger: base.Add(30 * time.Second), unitActive: base.Add(31 * time.Second)}, base.Add(10*time.Minute + 31*time.Second), true},
		{"calendar", unit.TimerConfig{OnCalendar: []unit.Calendar{cal}}, scheduleInputs{activated: base}, base.Add(14 * time.Hour), true},
		{"persistent catch-up", unit.TimerConfig{OnCalendar: []unit.Calendar{cal}, Persistent: true}, scheduleInputs{activated: base, persisted: base.Add(-72 * time.Hour)}, base.Add(-58 * time.Hour), true},
		{"randomized", unit.TimerConfig{OnActive: []time.Duration{time.Minute}}, scheduleInputs{activated: base, randomization: 5 * time.Second}, base.Add(65 * time.Second), true},
	}
	for _, c := range cases {
		got, ok := nextElapse(&c.timer, c.in)
		if ok != c.ok || !got.Equal(c.want) {
			t.Errorf("%s: nextElapse = %v, %v; want %v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestShouldRestart(t *testing.T) {
	s := &unit.ServiceConfig{SuccessExitStatus: unit.ExitSet{Codes: []int{42}}}
	cases := []struct {
		policy unit.RestartPolicy
		status exitStatus
		want   bool
	}{
		{unit.RestartOnFailure, exitStatus{code: 1}, true},
		{unit.RestartOnFailure, exitStatus{code: 0}, false},
		{unit.RestartOnFailure, exitStatus{code: 42}, false},
		{unit.RestartOnFailure, exitStatus{signal: syscall.SIGTERM}, false},
		{unit.RestartOnFailure, exitStatus{signal: syscall.SIGSEGV}, true},
		{unit.RestartOnAbnormal, exitStatus{code: 1}, false},
		{unit.RestartOnAbnormal, exitStatus{timedOut: true}, true},
		{unit.RestartOnSuccess, exitStatus{code: 0}, true},
		{unit.RestartAlways, exitStatus{code: 0}, true},
		{unit.RestartNo, exitStatus{code: 1}, false},
	}
	for _, c := range cases {
		s.Restart = c.policy
		if got := shouldRestart(s, c.status); got != c.want {
			t.Errorf("shouldRestart(%v, %+v) = %v, want %v", c.policy, c.status, got, c.want)
		}
	}
}

func TestDetectLayoutRejectsLongSocketPaths(t *testing.T) {
	long := filepath.Join("/tmp", strings.Repeat("x", maxSocketPath))
	env := map[string]string{"HOME": t.TempDir(), "XDG_RUNTIME_DIR": long}
	_, err := DetectLayout(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "set XDG_RUNTIME_DIR") {
		t.Fatalf("DetectLayout with a %d-byte runtime dir = %v, want a socket path length error", len(long), err)
	}
	env["XDG_RUNTIME_DIR"] = "/run/user/10123"
	l, err := DetectLayout(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if l.Socket() != "/run/user/10123/phix/supervisor.sock" {
		t.Errorf("socket = %s", l.Socket())
	}
}

func TestFollowStreamsNewLines(t *testing.T) {
	h := newHarness(t)
	h.install("chatty.service", "[Service]\nExecStart=/bin/sh -c 'i=0; while :; do i=$((i+1)); echo line $i; sleep 0.05; done'\n", "default.target")
	h.start()
	h.waitState("chatty.service", "active", "running")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got []string
	err := h.client.Stream(ctx, Request{Op: OpFollow, Units: []string{"chatty"}, Lines: 1}, func(r Response) error {
		if strings.Contains(r.Line, " line ") {
			got = append(got, r.Line)
		}
		if len(got) == 4 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("follow ended with %v, want context.Canceled", err)
	}
	if len(got) != 4 {
		t.Errorf("followed %d lines, want 4: %q", len(got), got)
	}
}

func TestSocketActivation(t *testing.T) {
	h := newHarness(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(h.layout.Host.RuntimeDir, "echo", "echo.sock")
	h.install("echo.socket", "[Socket]\nListenStream=%t/echo/echo.sock\nSocketMode=0600\nFileDescriptorName=greeter\nBacklog=16\n", "sockets.target")
	h.install("echo.service", fmt.Sprintf("[Service]\nEnvironment=%s=socket\nExecStart=%s\n", helperEnv, self))
	h.start()
	s := h.waitState("echo.socket", "active", "listening")
	if s.Triggers != "echo.service" || !slices.Equal(s.Listen, []string{sock}) || !slices.Equal(s.Ignored, []string{"Socket.Backlog"}) {
		t.Errorf("socket status = %+v", s)
	}
	if svc := h.status("echo.service"); svc.Active != "inactive" {
		t.Fatalf("echo.service started before any connection: %+v", svc)
	}
	info, err := os.Stat(sock)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket file %s: %v, mode %v", sock, err, info)
	}
	read := func() string {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		buf := make([]byte, 64)
		if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("reading from the activated service: %v; log:\n%s", err, strings.Join(h.logs("echo.service"), "\n"))
		}
		return string(buf[:n])
	}
	if got := read(); got != "hello greeter\n" {
		t.Errorf("first connection got %q", got)
	}
	h.waitState("echo.service", "active", "running")
	if got := read(); got != "hello greeter\n" {
		t.Errorf("second connection got %q", got)
	}
	h.control(OpStop, "echo.service")
	h.waitState("echo.socket", "active", "listening")
	if got := read(); got != "hello greeter\n" {
		t.Errorf("connection after the service stopped got %q", got)
	}
	h.control(OpStop, "echo.socket")
	if _, err := os.Stat(sock); err != nil {
		t.Errorf("the socket file went away without RemoveOnStop: %v", err)
	}
}

func TestSocketFailsWhenItsServiceCannotStart(t *testing.T) {
	h := newHarness(t)
	h.install("dead.socket", "[Socket]\nListenStream=%t/dead.sock\n", "sockets.target")
	h.install("dead.service", "[Service]\nExecStart=/nonexistent/phix-binary\n")
	h.start()
	h.waitState("dead.socket", "active", "listening")
	conn, err := net.Dial("unix", filepath.Join(h.layout.Host.RuntimeDir, "dead.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	h.waitState("dead.socket", "failed", "failed")
}

func TestNamedExitStatusAndExecPath(t *testing.T) {
	h := newHarness(t)
	bin := filepath.Join(h.layout.Host.Home, "tools")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "phix-datafail"), []byte("#!/bin/sh\nexit 65\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.install("tmpfiles.service", "[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=phix-datafail\nSuccessExitStatus=DATAERR\n", "default.target")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	readyCh := make(chan error, 1)
	go func() {
		h.done <- Supervise(ctx, h.layout, Program{Executable: self, ExecPath: []string{bin}}, os.Environ(), func(err error) { readyCh <- err })
	}()
	if err := <-readyCh; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.stop)
	h.waitState("tmpfiles.service", "active", "exited")
}
