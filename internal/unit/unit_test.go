package unit

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSplitWords(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`a b  c`, []string{"a", "b", "c"}},
		{`"a b" c`, []string{"a b", "c"}},
		{`'a "b"' c`, []string{`a "b"`, "c"}},
		{`foo"bar baz"`, []string{"foobar baz"}},
		{`a\ b \x41\n`, []string{"a b", "A\n"}},
		{`""`, []string{""}},
	}
	for _, c := range cases {
		got, err := splitWords(c.in)
		if err != nil {
			t.Fatalf("splitWords(%q): %v", c.in, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitWords(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{`"open`, `trailing\`, `\q`} {
		if _, err := splitWords(bad); err == nil {
			t.Errorf("splitWords(%q) succeeded, want an error", bad)
		}
	}
}

func testHost(root string) Host {
	return Host{
		Home:       filepath.Join(root, "home"),
		User:       "nix-on-droid",
		UID:        10123,
		GID:        10123,
		Hostname:   "phone.lan",
		ConfigHome: filepath.Join(root, "home", ".config"),
		DataHome:   filepath.Join(root, "home", ".local", "share"),
		StateHome:  filepath.Join(root, "home", ".local", "state"),
		CacheHome:  filepath.Join(root, "home", ".cache"),
		RuntimeDir: filepath.Join(root, "run"),
		TempDir:    filepath.Join(root, "tmp"),
	}
}

func TestParseCommands(t *testing.T) {
	n, _ := ParseName("web@site-a.service")
	spec := specifiers{host: testHost("/r"), name: n}
	cmds, err := parseCommands(`-@/bin/srv srv-%i --root %h/%I "two words" $ARGS ${ONE}x ; :/bin/echo $NOPE`, spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 2 {
		t.Fatalf("got %d commands, want 2", len(cmds))
	}
	env := Env{"ARGS": "a  b", "ONE": "1", "NOPE": "x"}
	got := cmds[0].Argv(env.Lookup)
	want := []string{"srv-site-a", "--root", "/r/home/site/a", "two words", "a", "b", "1x"}
	if !reflect.DeepEqual(got, want) || cmds[0].Path != "/bin/srv" || !cmds[0].IgnoreFailure {
		t.Errorf("first command = %+v argv %q, want path /bin/srv, ignore failure, argv %q", cmds[0], got, want)
	}
	if got := cmds[1].Argv(env.Lookup); !reflect.DeepEqual(got, []string{"/bin/echo", "$NOPE"}) {
		t.Errorf("second command argv = %q, want no expansion", got)
	}
	if _, err := parseCommands(`./relative`, spec); err == nil {
		t.Error("a relative executable path parsed, want an error")
	}
}

func TestParseSpan(t *testing.T) {
	cases := map[string]time.Duration{
		"5":         5 * time.Second,
		"5min":      5 * time.Minute,
		"1h 30min":  90 * time.Minute,
		"2h30m":     150 * time.Minute,
		"100ms":     100 * time.Millisecond,
		"1.5h":      90 * time.Minute,
		"1w":        7 * 24 * time.Hour,
		"10s 500ms": 10500 * time.Millisecond,
	}
	for in, want := range cases {
		got, err := parseSpan(in)
		if err != nil || got != want {
			t.Errorf("parseSpan(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if d, err := parseTimeout("infinity"); err != nil || d != 0 {
		t.Errorf("parseTimeout(infinity) = %v, %v; want 0 (no timeout)", d, err)
	}
	for _, bad := range []string{"", "5 parsecs", "min"} {
		if _, err := parseSpan(bad); err == nil {
			t.Errorf("parseSpan(%q) succeeded, want an error", bad)
		}
	}
}

func TestCalendarNext(t *testing.T) {
	from := time.Date(2026, 11, 21, 10, 30, 15, 0, time.UTC)
	cases := []struct {
		spec string
		want []time.Time
	}{
		{"weekly", []time.Time{time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)}},
		{"daily", []time.Time{time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)}},
		{"hourly", []time.Time{time.Date(2026, 11, 21, 11, 0, 0, 0, time.UTC)}},
		{"*:0/15", []time.Time{time.Date(2026, 11, 21, 10, 45, 0, 0, time.UTC), time.Date(2026, 11, 21, 11, 0, 0, 0, time.UTC)}},
		{"Mon..Fri 09:00", []time.Time{time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)}},
		{"Sat,Sun *-*-* 12:00:00", []time.Time{time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC), time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}},
		{"*-02~01 03:00", []time.Time{time.Date(2027, 2, 28, 3, 0, 0, 0, time.UTC), time.Date(2028, 2, 29, 3, 0, 0, 0, time.UTC)}},
		{"*-*-01 00:00:00", []time.Time{time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)}},
		{"2027-01-01 00:00 UTC", []time.Time{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}},
		{"quarterly", []time.Time{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)}},
	}
	for _, c := range cases {
		cal, err := ParseCalendar(c.spec, time.UTC)
		if err != nil {
			t.Fatalf("ParseCalendar(%q): %v", c.spec, err)
		}
		at := from
		for i, want := range c.want {
			got, ok := cal.Next(at)
			if !ok || !got.Equal(want) {
				t.Errorf("%q elapse %d after %v = %v (ok %v), want %v", c.spec, i, at, got, ok, want)
				break
			}
			at = got
		}
	}
	for _, bad := range []string{"", "*-02-30", "Funday", "25:00", "*-*-* 00:00:00.5", "*-13-01"} {
		if _, err := ParseCalendar(bad, time.UTC); err == nil {
			t.Errorf("ParseCalendar(%q) succeeded, want an error", bad)
		}
	}
}

func TestCalendarLocalTime(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	cal, err := ParseCalendar("*-*-* 02:30:00", berlin)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := cal.Next(time.Date(2026, 3, 28, 12, 0, 0, 0, berlin))
	if !ok || got.Before(time.Date(2026, 3, 29, 0, 0, 0, 0, berlin)) {
		t.Errorf("next 02:30 across the spring DST gap = %v, %v; want a time on 2026-03-29 or later", got, ok)
	}
	if _, ok := cal.Next(time.Date(2026, 10, 25, 1, 0, 0, 0, berlin)); !ok {
		t.Error("next 02:30 across the autumn DST fold found nothing")
	}
}

func TestEnvironmentFormats(t *testing.T) {
	vars, err := ParseEnvironmentFile([]byte("# comment\nA=1\nB=\"two words\\n\"\nC = spaced \nD=a\\\nb\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Var{{"A", "1"}, {"B", "two words\n"}, {"C", "spaced"}, {"D", "ab"}}
	if !reflect.DeepEqual(vars, want) {
		t.Errorf("ParseEnvironmentFile = %q, want %q", vars, want)
	}
	env := Env{"XDG_RUNTIME_DIR": "/run/u", "LADSPA_PATH": "/old"}
	if err := ParseEnvironmentD([]byte("TMUX_TMPDIR=${XDG_RUNTIME_DIR}/tmux\nLADSPA_PATH=/new${LADSPA_PATH:+:$LADSPA_PATH}\nX=${UNSET:-dflt}\n"), env); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"TMUX_TMPDIR": "/run/u/tmux", "LADSPA_PATH": "/new:/old", "X": "dflt"} {
		if env[k] != v {
			t.Errorf("environment.d %s = %q, want %q", k, env[k], v)
		}
	}
}

type tree struct {
	t    *testing.T
	root string
	host Host
}

func newTree(t *testing.T) *tree {
	root := t.TempDir()
	tr := &tree{t: t, root: root, host: testHost(root)}
	return tr
}

func (tr *tree) unitDir() string { return tr.host.UnitDirs()[0] }

func (tr *tree) write(rel, content string) string {
	tr.t.Helper()
	path := filepath.Join(tr.root, "store", rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		tr.t.Fatal(err)
	}
	return path
}

func (tr *tree) link(rel, target string) {
	tr.t.Helper()
	path := filepath.Join(tr.unitDir(), rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr *tree) install(name, content string, wantedBy ...string) {
	tr.t.Helper()
	target := tr.write(name, content)
	tr.link(name, target)
	for _, w := range wantedBy {
		tr.link(w+".wants/"+name, target)
	}
}

func (tr *tree) load() *Set {
	tr.t.Helper()
	set, err := Load(tr.host, time.UTC)
	if err != nil {
		tr.t.Fatal(err)
	}
	return set
}

func name(t *testing.T, s string) Name {
	t.Helper()
	n, err := ParseName(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLoadHomeManagerLayout(t *testing.T) {
	tr := newTree(t)
	tr.install("srv-daemon.service", `[Install]
WantedBy=default.target

[Service]
Environment=PATH=/run/wrappers/bin:/run/current-system/sw/bin
Environment=XDG_CONFIG_HOME=%h/.config
ExecStart=/nix/store/x-srv/bin/srv daemon start --foreground
Restart=on-failure
RestartSec=5
Type=simple
IOSchedulingClass=idle
ProtectSystem=strict

[Unit]
Description=srv daemon
Documentation=https://example.invalid
After=sops-nix.service
Wants=sops-nix.service
`, "default.target")
	tr.install("sops-nix.service", `[Service]
Environment='PATH='
ExecStart=/nix/store/x-sops-nix-user
Type=oneshot
[Unit]
Description=sops-nix activation
`, "default.target")
	tr.install("mail-sync.timer", `[Timer]
OnStartupSec=30s
OnUnitActiveSec=5min
OnCalendar=weekly
Persistent=true
[Unit]
Description=Periodic mail sync
`, "timers.target")
	tr.install("mail-sync.service", "[Service]\nType=oneshot\nExecStart=/bin/true\n")
	tr.link("srv-daemon.service.d/override.conf", tr.write("override.conf", "[Service]\nRestartSec=\\\n  7\n"))

	set := tr.load()
	if len(set.Errors) != 0 {
		t.Fatalf("load errors: %v", set.Errors)
	}
	srv := set.Units[name(t, "srv-daemon.service")]
	if srv == nil {
		t.Fatal("srv-daemon.service was not loaded")
	}
	s := srv.Service
	if s.Restart != RestartOnFailure || s.RestartDelay != 7*time.Second || s.Type != Simple || *s.IOSchedulingClass != 3 {
		t.Errorf("srv-daemon service = %+v, want on-failure, 7s from the drop-in, simple, idle IO class", s)
	}
	if got := s.Environment[1].Value; got != filepath.Join(tr.host.Home, ".config") {
		t.Errorf("%%h expanded to %q", got)
	}
	if !slices.Equal(srv.Ignored, []string{"Service.ProtectSystem", "Unit.Documentation"}) {
		t.Errorf("ignored = %q", srv.Ignored)
	}
	def := set.Units[name(t, "default.target")]
	if def == nil || !slices.Contains(def.Deps.Wants, name(t, "srv-daemon.service")) || !slices.Contains(def.Deps.After, name(t, "sops-nix.service")) {
		t.Errorf("default.target deps = %+v, want Wants and After on its .wants members", def)
	}
	if set.Units[name(t, "basic.target")] == nil || set.Units[name(t, "timers.target")] == nil {
		t.Error("the builtin basic.target and timers.target were not synthesised")
	}
	timer := set.Units[name(t, "mail-sync.timer")].Timer
	if timer.Unit != name(t, "mail-sync.service") || !timer.Persistent || len(timer.OnCalendar) != 1 || timer.OnStartup[0] != 30*time.Second {
		t.Errorf("timer = %+v", timer)
	}
	if set.Units[name(t, "sops-nix.service")].Service.TimeoutStart != 0 {
		t.Error("a oneshot service kept the 90s start timeout, want none")
	}
}

func TestLoadDigestTracksContent(t *testing.T) {
	tr := newTree(t)
	path := tr.write("a.service", "[Service]\nExecStart=/bin/a\n")
	tr.link("a.service", path)
	before := tr.load().Units[name(t, "a.service")].Digest
	if err := os.WriteFile(path, []byte("[Service]\nExecStart=/bin/b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if after := tr.load().Units[name(t, "a.service")].Digest; after == before {
		t.Error("the digest did not change with the unit file's content")
	}
}

func TestLoadErrorsArePerUnit(t *testing.T) {
	tr := newTree(t)
	tr.install("good.service", "[Service]\nExecStart=/bin/true\n[Unit]\nWants=missing.service\n", "default.target")
	tr.install("forking.service", "[Service]\nType=forking\nExecStart=/bin/true\n", "default.target")
	tr.install("unknown.service", "[Service]\nExecStart=/bin/true\nFrobnicate=yes\n")
	tr.install("watch.path", "[Path]\nPathChanged=%h/x\n", "paths.target")
	tr.install("accept.socket", "[Socket]\nListenStream=%t/a\nAccept=yes\n", "sockets.target")
	tr.install("user.service", "[Service]\nExecStart=/bin/true\nUser=root\n")

	set := tr.load()
	if set.Units[name(t, "good.service")] == nil {
		t.Fatalf("good.service failed to load: %v", set.Errors[name(t, "good.service")])
	}
	for unitName, fragment := range map[string]string{
		"forking.service": `service type "forking" is not supported`,
		"unknown.service": "Frobnicate= is not supported",
		"watch.path":      "path units are not supported",
		"accept.socket":   "(Accept=yes)",
		"missing.service": "unit file not found",
		"user.service":    "User= is not supported",
	} {
		err := set.Errors[name(t, unitName)]
		if err == nil || !strings.Contains(err.Error(), fragment) {
			t.Errorf("%s error = %v, want one mentioning %q", unitName, err, fragment)
		}
	}
}

func TestLoadTemplateInstances(t *testing.T) {
	tr := newTree(t)
	target := tr.write("web@.service", "[Service]\nExecStart=/bin/web --site %i --dir %I\nKillSignal=SIGINT\n")
	tr.link("web@.service", target)
	tr.link("default.target.wants/web@blog-a.service", target)
	set := tr.load()
	u := set.Units[name(t, "web@blog-a.service")]
	if u == nil {
		t.Fatalf("instance failed to load: %v", set.Errors)
	}
	argv := u.Service.ExecStart[0].Argv(Env{}.Lookup)
	if !slices.Equal(argv, []string{"/bin/web", "--site", "blog-a", "--dir", "blog/a"}) || u.Service.KillSignal != syscall.SIGINT {
		t.Errorf("instance argv = %q kill %v", argv, u.Service.KillSignal)
	}
	if _, loaded := set.Units[name(t, "web@.service")]; loaded {
		t.Error("the template itself was loaded as a unit")
	}
}

func TestConditions(t *testing.T) {
	dir := t.TempDir()
	host := testHost(dir)
	env := Env{"MODE": "phone"}
	cond := func(kind ConditionKind, arg string, negate, trig bool) Condition {
		return Condition{Key: "Condition", Kind: kind, Arg: arg, Negate: negate, Triggered: trig}
	}
	cases := []struct {
		conds []Condition
		want  bool
	}{
		{[]Condition{cond(CondPathExists, dir, false, false)}, true},
		{[]Condition{cond(CondPathExists, dir, true, false)}, false},
		{[]Condition{cond(CondEnvironment, "MODE=phone", false, false)}, true},
		{[]Condition{cond(CondEnvironment, "MODE=desk", false, false)}, false},
		{[]Condition{cond(CondPathExists, "/nonexistent", false, true), cond(CondEnvironment, "MODE", false, true)}, true},
		{[]Condition{cond(CondPathExists, "/nonexistent", false, true), cond(CondHost, "desk*", false, true)}, false},
		{[]Condition{cond(CondHost, "phone*", false, false), cond(CondUser, "10123", false, false)}, true},
	}
	for i, c := range cases {
		if got, why := CheckConditions(c.conds, env, host); got != c.want {
			t.Errorf("case %d: CheckConditions = %v (%s), want %v", i, got, why, c.want)
		}
	}
}

func TestLoadSocket(t *testing.T) {
	tr := newTree(t)
	tr.install("tmux.socket", "[Unit]\nDescription=tmux socket\n[Socket]\nListenStream=%t/tmux/tmux-%U/default\nListenDatagram=127.0.0.1:5353\nListenStream=8080\nSocketMode=0600\nDirectoryMode=0700\n[Install]\nWantedBy=sockets.target\n", "sockets.target")
	tr.install("tmux.service", "[Service]\nExecStart=/bin/tmux -D\n")
	set := tr.load()
	u := set.Units[name(t, "tmux.socket")]
	if u == nil {
		t.Fatalf("tmux.socket failed: %v", set.Errors[name(t, "tmux.socket")])
	}
	s := u.Socket
	want := []Listen{
		{Kind: ListenStream, Unix: filepath.Join(tr.host.RuntimeDir, "tmux", "tmux-10123", "default")},
		{Kind: ListenDatagram, Address: "127.0.0.1:5353"},
		{Kind: ListenStream, Address: ":8080"},
	}
	if !reflect.DeepEqual(s.Listen, want) || s.SocketMode != 0o600 || s.DirectoryMode != 0o700 || s.Service != name(t, "tmux.service") || s.FDName != "tmux.socket" {
		t.Errorf("socket = %+v", s)
	}
	if !slices.Contains(u.Deps.Before, name(t, "tmux.service")) {
		t.Error("the socket is not ordered before its service")
	}
	if set.Units[name(t, "tmux.service")] == nil {
		t.Error("the socket's service was not loaded")
	}
}

func TestNamedExitStatuses(t *testing.T) {
	var set ExitSet
	if err := parseExitSet(&set, "DATAERR 3 SIGUSR1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(set.Codes, []int{65, 3}) || !slices.Equal(set.Signals, []syscall.Signal{syscall.SIGUSR1}) {
		t.Errorf("exit set = %+v", set)
	}
}

func TestParseUMask(t *testing.T) {
	for in, want := range map[string]UMask{"0": 0, "022": 0o22, "0077": 0o77, "777": 0o777} {
		got, err := ParseUMask(in)
		if err != nil || got != want {
			t.Errorf("ParseUMask(%q) = %o, %v; want %o", in, got, err, want)
		}
		if round, err := ParseUMask(got.String()); err != nil || round != got {
			t.Errorf("ParseUMask(%q.String()) = %o, %v; want %o", in, round, err, got)
		}
	}
	for _, in := range []string{"1000", "7777", "37777777777", "77777777777777777777777", "-1", "8", "", "0x12"} {
		if got, err := ParseUMask(in); err == nil || !strings.Contains(err.Error(), strconv.Quote(in)) {
			t.Errorf("ParseUMask(%q) = %o, %v; want an error naming the input", in, got, err)
		}
	}
}

func TestUMaskDirective(t *testing.T) {
	tr := newTree(t)
	tr.install("masked.service", "[Service]\nExecStart=/bin/true\nUMask=0027\n", "default.target")
	tr.install("wide.service", "[Service]\nExecStart=/bin/true\nUMask=1000\n", "default.target")
	tr.install("huge.service", "[Service]\nExecStart=/bin/true\nUMask=40000000000\n", "default.target")

	set := tr.load()
	masked := set.Units[name(t, "masked.service")]
	if masked == nil {
		t.Fatalf("masked.service failed to load: %v", set.Errors[name(t, "masked.service")])
	}
	if got := masked.Service.UMask; got == nil || *got != 0o27 {
		t.Errorf("masked.service UMask = %v, want 27", got)
	}
	for _, unitName := range []string{"wide.service", "huge.service"} {
		if err := set.Errors[name(t, unitName)]; err == nil || !strings.Contains(err.Error(), "0..0777") {
			t.Errorf("%s error = %v, want an out-of-range umask error", unitName, err)
		}
	}
}
