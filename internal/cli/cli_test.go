package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/stubbedev/phix/internal/config"
	"github.com/stubbedev/phix/internal/proc"
	"github.com/stubbedev/phix/internal/rebuild"
	"github.com/stubbedev/phix/internal/runlock"
	"github.com/stubbedev/phix/internal/ui"
	"github.com/stubbedev/phix/internal/usage"
	"github.com/stubbedev/phix/internal/version"
)

var sopsFixture = mustAbs(filepath.Join("..", "sops", "testdata", "sops.yaml"))

func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		panic(err)
	}
	return abs
}

type harness struct {
	t     *testing.T
	dir   string
	flake string
	home  string
}

func requireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("%s is required on PATH for this test: %v", name, err)
		}
	}
}

func newHarness(t *testing.T, commands ...map[string]any) *harness {
	t.Helper()
	return newHarnessWith(t, nil, commands...)
}

func newHarnessWith(t *testing.T, postSwitch []map[string]any, commands ...map[string]any) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, dir: dir, flake: filepath.Join(dir, "flake"), home: filepath.Join(dir, "home")}
	for _, d := range []string{filepath.Join(h.flake, "secrets"), filepath.Join(h.home, ".ssh"), filepath.Join(dir, "elsewhere")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sopsYAML, err := os.ReadFile(sopsFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.flake, ".sops.yaml"), sopsYAML, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "flake-link")
	if err := os.Symlink(h.flake, link); err != nil {
		t.Fatal(err)
	}
	if commands == nil {
		commands = []map[string]any{}
	}
	cfg, err := yaml.Marshal(map[string]any{
		"flake":    "/nonexistent",
		"platform": "nixos",
		"attr":     "desktop",
		"ssh_key":  filepath.Join(h.home, ".ssh", "id_ed25519"),
		"hooks":    map[string]any{"post_switch": postSwitch},
		"commands": commands,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", h.home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(h.home, ".cache"))
	t.Setenv("PHIX_CONFIG", cfgPath)
	t.Setenv("PHIX_FLAKE_DIR", link)
	t.Setenv("SOPS_AGE_KEY_FILE", "")
	t.Chdir(filepath.Join(dir, "elsewhere"))
	return h
}

func (h *harness) run(stdin string, args ...string) (string, string, int) {
	h.t.Helper()
	var out, errOut bytes.Buffer
	app := New(proc.Streams{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut}, Terminal{})
	code := app.Run(context.Background(), args)
	return out.String(), errOut.String(), code
}

func (h *harness) mustRun(stdin string, args ...string) string {
	h.t.Helper()
	out, errOut, code := h.run(stdin, args...)
	if code != 0 {
		h.t.Fatalf("phix %v exited %d\nstdout: %s\nstderr: %s", args, code, out, errOut)
	}
	return out
}

func (h *harness) recipients() ([]string, map[string]string) {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.flake, ".sops.yaml"))
	if err != nil {
		h.t.Fatal(err)
	}
	var doc struct {
		Rules []struct {
			Groups []struct {
				Age yaml.Node `yaml:"age"`
			} `yaml:"key_groups"`
		} `yaml:"creation_rules"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		h.t.Fatal(err)
	}
	var keys []string
	comments := map[string]string{}
	for _, r := range doc.Rules {
		for _, g := range r.Groups {
			for _, n := range g.Age.Content {
				keys = append(keys, n.Value)
				comments[n.Value] = strings.TrimSpace(strings.TrimPrefix(n.LineComment, "#"))
			}
		}
	}
	return keys, comments
}

func command(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return strings.TrimSpace(string(out))
}

func TestHelp(t *testing.T) {
	h := newHarness(t)
	out := h.mustRun("", "help")
	for _, want := range []string{"Usage:", "Rebuild:", "Maintenance:", "switch", "dry-activate", "trust", "secret", "service", "PHIX_FLAKE_DIR"} {
		if !strings.Contains(out, want) {
			t.Errorf("help lacks %q:\n%s", want, out)
		}
	}
	if out, _, code := h.run("", "switch", "--help"); code != 0 || !strings.Contains(out, "--install-bootloader") || !strings.Contains(out, "--show-trace") {
		t.Errorf("switch --help exited %d:\n%s", code, out)
	}
	if out, _, code := h.run("", "build", "--help"); code != 0 || strings.Contains(out, "--install-bootloader") {
		t.Errorf("build --help offers --install-bootloader:\n%s", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("help is colored without a color terminal:\n%q", out)
	}
	if out := h.mustRun("", "--version"); out != "phix version "+version.Version+"\n" {
		t.Errorf("phix --version = %q", out)
	}
}

func TestFlagHelpIsAlignedWithoutBoolValueNoise(t *testing.T) {
	h := newHarness(t)
	out := h.mustRun("", "switch", "--help")
	if strings.Contains(out, "[=true|false]") {
		t.Fatalf("switch --help shows pflag's boolean value hint:\n%s", out)
	}
	section := out[strings.Index(out, "Flags:"):strings.Index(out, "Global Flags:")]
	column := -1
	for line := range strings.Lines(section) {
		trimmed := strings.TrimLeft(line, " ")
		if !strings.HasPrefix(trimmed, "-") {
			continue
		}
		spec := strings.Index(line, trimmed)
		desc := spec + strings.Index(line[spec:], "   ")
		desc += len(line[desc:]) - len(strings.TrimLeft(line[desc:], " "))
		if column == -1 {
			column = desc
		}
		if desc != column {
			t.Fatalf("flag descriptions start at column %d and %d:\n%s", column, desc, section)
		}
	}
	if column == -1 {
		t.Fatalf("no flags in switch --help:\n%s", out)
	}
}

func TestColoredOutput(t *testing.T) {
	h := newHarness(t)
	colored := Terminal{Stdout: ui.NewStyle(true, nil), Stderr: ui.NewStyle(true, nil)}
	run := func(args ...string) (string, string, int) {
		var out, errOut bytes.Buffer
		code := New(proc.Streams{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut}, colored).Run(context.Background(), args)
		return out.String(), errOut.String(), code
	}

	plain := h.mustRun("", "switch", "--help")
	out, _, code := run("switch", "--help")
	if code != 0 || ui.Strip(out) != plain {
		t.Fatalf("colored help differs from plain once stripped (exit %d):\n%s", code, ui.Strip(out))
	}
	for _, want := range []string{
		"\x1b[1;33mUsage:\x1b[0m",
		"\x1b[1;33mGlobal Flags:\x1b[0m",
		"\x1b[1;31m-L, --print-build-logs\x1b[0m",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("colored switch --help lacks %q:\n%q", want, out)
		}
	}
	if out, _, _ := run("help"); !strings.Contains(out, "\x1b[1;32mswitch ") || !strings.Contains(out, "\x1b[1;33mEnvironment:\x1b[0m") {
		t.Errorf("colored help does not highlight commands and headings:\n%q", out)
	}

	_, errOut, code := run()
	if code != 2 || !strings.Contains(errOut, "\x1b[1;33mUsage:\x1b[0m") || !strings.Contains(errOut, "\x1b[1;31merror:\x1b[0m no command given") {
		t.Fatalf("bare phix exited %d with stderr:\n%q", code, errOut)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	h := newHarness(t)
	cases := [][]string{
		{},
		{"frobnicate"},
		{"rollback", "extra"},
		{"secret"},
		{"secret", "shred", "x"},
		{"secret", "set"},
		{"generations", "--bogus"},
		{"switch", "--bogus"},
		{"switch", "extra"},
		{"build", "--install-bootloader"},
		{"switch", "--option", "noequals"},
		{"rollback", "x"},
		{"rollback", "0"},
		{"rollback", "1", "2"},
		{"--attr", "a.b", "generations"},
		{"gc", "stray"},
		{"mcp", "stray"},
		{"mcp", "--transport", "carrier-pigeon"},
		{"mcp", "--addr", "127.0.0.1:1"},
		{"mcp", "--path", "/x"},
		{"mcp", "--transport", "http", "--addr", ""},
		{"mcp", "--transport", "sse", "--path", "x"},
	}
	for _, args := range cases {
		if _, errOut, code := h.run("", args...); code != 2 {
			t.Errorf("phix %v exited %d, want 2; stderr: %s", args, code, errOut)
		}
	}
}

func TestMissingConfig(t *testing.T) {
	newHarness(t)
	t.Setenv("PHIX_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	var out, errOut bytes.Buffer
	code := New(proc.Streams{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut}, Terminal{}).Run(context.Background(), []string{"switch"})
	if code != 1 || !strings.Contains(errOut.String(), "missing.json") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	code = New(proc.Streams{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut}, Terminal{}).Run(context.Background(), []string{"completion", "zsh"})
	if code != 0 || !strings.HasPrefix(out.String(), "#compdef phix") {
		t.Fatalf("completion without a config: exit %d, output %q", code, out.String())
	}
}

type hangUpAfter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	reply  string
	hangUp func()
}

func (w *hangUpAfter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.reply) {
		w.hangUp()
	}
	return n, err
}

func TestMCPServesStdioWithoutConfig(t *testing.T) {
	newHarness(t)
	t.Setenv("PHIX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	in, client := io.Pipe()
	out := &hangUpAfter{reply: `"id":2,"result"`, hangUp: func() { client.Close() }}
	go func() {
		for _, req := range []string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		} {
			if _, err := io.WriteString(client, req+"\n"); err != nil {
				return
			}
		}
	}()
	var errOut bytes.Buffer
	code := New(proc.Streams{Stdin: in, Stdout: out, Stderr: &errOut}, Terminal{}).Run(context.Background(), []string{"mcp"})
	if code != 0 {
		t.Fatalf("phix mcp exited %d: %s", code, errOut.String())
	}
	for _, want := range []string{`"name":"phix-mcp"`, `"name":"nix"`, `"name":"nix_versions"`} {
		if !strings.Contains(out.buf.String(), want) {
			t.Errorf("phix mcp output lacks %s:\n%s", want, out.buf.String())
		}
	}
}

func TestMCPTokenRequiresHTTP(t *testing.T) {
	h := newHarness(t)
	t.Setenv(mcpTokenEnv, "secret")
	if _, errOut, code := h.run("", "mcp"); code != 2 || !strings.Contains(errOut, mcpTokenEnv) {
		t.Fatalf("phix mcp with a token over stdio exited %d: %s", code, errOut)
	}
}

func TestExternalCommands(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "phix-echo")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"args:$*\"\nexit \"${EXIT:-0}\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, map[string]any{
		"name": "echo", "description": "Echo things", "exec": script,
		"complete": []map[string]string{{"name": "loud", "description": "Shout"}},
	})
	if out := h.mustRun("", "echo", "a", "--flag"); out != "args:a --flag\n" {
		t.Fatalf("external output %q", out)
	}
	if out := h.mustRun("", "help"); !strings.Contains(out, "Extra:") || !strings.Contains(out, "Echo things") {
		t.Fatalf("help lacks the external command:\n%s", out)
	}
	if out := h.mustRun("", "__complete", "echo", ""); !strings.Contains(out, "loud\tShout") {
		t.Fatalf("completion lacks the external choices:\n%s", out)
	}
	t.Setenv("EXIT", "7")
	if _, errOut, code := h.run("", "echo"); code != 7 || strings.Contains(errOut, "phix:") {
		t.Fatalf("external failure: exit %d, stderr %q", code, errOut)
	}
}

func TestExternalCollision(t *testing.T) {
	h := newHarness(t, map[string]any{"name": "switch", "description": "", "exec": "/bin/true"})
	if _, errOut, code := h.run("", "help"); code != 1 || !strings.Contains(errOut, "collides") {
		t.Fatalf("collision: exit %d, stderr %q", code, errOut)
	}
}

func TestSecretCompletion(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{"github-token", ".hidden"} {
		if err := os.WriteFile(filepath.Join(h.flake, "secrets", name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := h.mustRun("", "__complete", "secret", "edit", "")
	if !strings.Contains(out, "github-token") || strings.Contains(out, ".hidden") {
		t.Fatalf("secret completion:\n%s", out)
	}
}

func TestMutatingCommandsAreExclusive(t *testing.T) {
	h := newHarness(t)
	dir, err := config.CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := runlock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	for _, args := range [][]string{{"update"}, {"switch"}, {"secret", "rotate", "token"}, {"gc"}} {
		_, errOut, code := h.run("", args...)
		if code == 0 || !strings.Contains(errOut, "another phix run (pid ") {
			t.Errorf("phix %v with the lock held exited %d:\n%s", args, code, errOut)
		}
	}
	if _, errOut, code := h.run("", "whoami"); strings.Contains(errOut, "another phix run") {
		t.Errorf("read-only whoami blocked by the lock (exit %d):\n%s", code, errOut)
	}
}

func TestTrustAndSecrets(t *testing.T) {
	requireTools(t, "sops", "age", "age-keygen", "ssh-to-age", "ssh-keygen")
	h := newHarness(t)

	firstKey := filepath.Join(h.dir, "first.key")
	secondKey := filepath.Join(h.dir, "second.key")
	command(t, "age-keygen", "-o", firstKey)
	command(t, "age-keygen", "-o", secondKey)
	first := command(t, "age-keygen", "-y", firstKey)
	second := command(t, "age-keygen", "-y", secondKey)
	before, _ := h.recipients()

	out := h.mustRun("", "trust", "first", first)
	keys, comments := h.recipients()
	if strings.Join(keys, " ") != strings.Join(append(before, first), " ") {
		t.Fatalf("trust did not append %s: %v", first, keys)
	}
	if comments[first] != "first" {
		t.Fatalf("trust labelled %s as %q", first, comments[first])
	}
	if !strings.Contains(out, `git -C "`+h.flake+`" diff`) {
		t.Fatalf("PHIX_FLAKE_DIR not resolved to %s:\n%s", h.flake, out)
	}
	if !strings.Contains(out, "\n✓ Trusted first across 0 secret file(s)\n  Review:") {
		t.Fatalf("trust summary misformatted:\n%s", out)
	}

	snapshot, _ := os.ReadFile(filepath.Join(h.flake, ".sops.yaml"))
	if out := h.mustRun("", "trust", "first", first); !strings.Contains(out, "already present") {
		t.Fatalf("re-trust not a no-op:\n%s", out)
	}
	if now, _ := os.ReadFile(filepath.Join(h.flake, ".sops.yaml")); !bytes.Equal(now, snapshot) {
		t.Fatal("re-trusting changed .sops.yaml")
	}

	h.mustRun("hunter2\n\n", "secret", "set", "token")
	t.Setenv("SOPS_AGE_KEY_FILE", firstKey)
	out, errOut, code := h.run("", "trust", "second", second)
	if code != 0 || !strings.Contains(out, "Re-wrapping token") || !strings.Contains(out, "Trusted second across 1 secret file(s)") {
		t.Fatalf("trust did not re-wrap token (exit %d):\n%s\n%s", code, out, errOut)
	}
	if want := "│ " + ui.Unicode.Added + " " + second + "\n"; !strings.Contains(errOut, want) {
		t.Fatalf("re-wrap does not list the added recipient %q:\n%s", want, errOut)
	}
	if want := "│ " + ui.Unicode.Done + " re-encrypted for the new recipients\n"; !strings.Contains(errOut, want) {
		t.Fatalf("re-wrap does not report the re-encryption:\n%s", errOut)
	}
	keys, _ = h.recipients()
	if strings.Join(keys, " ") != strings.Join(append(before, first, second), " ") {
		t.Fatalf("trust did not append %s: %v", second, keys)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", secondKey)
	plain := command(t, "sops", "--decrypt", "--input-type", "binary", "--output-type", "binary", filepath.Join(h.flake, "secrets", "token"))
	if plain != "hunter2" {
		t.Fatalf("secrets/token decrypts to %q for %s", plain, second)
	}

	if _, _, code := h.run("a\x00b", "secret", "set", "nul"); code != 1 {
		t.Fatalf("NUL value exited %d, want 1", code)
	}
	if _, err := os.Stat(filepath.Join(h.flake, "secrets", "nul")); !os.IsNotExist(err) {
		t.Fatal("a value with a NUL byte was written")
	}
	for _, name := range []string{"../escape", "/etc/escape", ".hidden", "sub/dir", ""} {
		if _, _, code := h.run("x", "secret", "set", name); code != 2 {
			t.Errorf("secret set %q exited %d, want 2", name, code)
		}
	}
	if _, err := os.Stat(filepath.Join(h.flake, "escape")); !os.IsNotExist(err) {
		t.Fatal("secret set ../escape wrote outside secrets/")
	}
	if entries, _ := os.ReadDir(filepath.Join(h.flake, "secrets")); len(entries) != 1 {
		t.Fatalf("secrets/ holds %v, want only token", entries)
	}

	if out := h.mustRun("", "secret", "rotate", "token"); !strings.Contains(out, "Re-rolled data key for token") {
		t.Fatalf("rotate output:\n%s", out)
	}
	if plain := command(t, "sops", "--decrypt", "--input-type", "binary", "--output-type", "binary", filepath.Join(h.flake, "secrets", "token")); plain != "hunter2" {
		t.Fatalf("rotated token decrypts to %q", plain)
	}
	if _, _, code := h.run("", "secret", "rotate", "absent"); code != 1 {
		t.Fatalf("rotating a missing secret exited %d, want 1", code)
	}

	if _, _, code := h.run("", "trust", "bad name", second); code != 2 {
		t.Fatalf("invalid trust name exited %d, want 2", code)
	}
	if _, _, code := h.run("", "trust", "x", "not-a-key"); code != 2 {
		t.Fatalf("invalid pubkey exited %d, want 2", code)
	}
}

func TestWhoamiPipesIntoTrust(t *testing.T) {
	requireTools(t, "sops", "age", "ssh-to-age", "ssh-keygen")
	h := newHarness(t)
	command(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(h.home, ".ssh", "id_ed25519"))
	own := command(t, "sh", "-c", "ssh-to-age < \"$1\"", "sh", filepath.Join(h.home, ".ssh", "id_ed25519.pub"))
	host, err := shortHostname()
	if err != nil {
		t.Fatal(err)
	}

	whoami := h.mustRun("", "whoami")
	if whoami != host+" "+own+"\n" {
		t.Fatalf("whoami printed %q", whoami)
	}
	h.mustRun(whoami, "trust")
	if _, comments := h.recipients(); comments[own] != host {
		t.Fatalf("piped trust labelled %s as %q, want %q", own, comments[own], host)
	}

	h2 := newHarness(t)
	command(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(h2.home, ".ssh", "id_ed25519"))
	own2 := command(t, "sh", "-c", "ssh-to-age < \"$1\"", "sh", filepath.Join(h2.home, ".ssh", "id_ed25519.pub"))
	h2.mustRun("", "trust", own2)
	if _, comments := h2.recipients(); comments[own2] != host {
		t.Fatalf("own key auto-named %q, want %q", comments[own2], host)
	}
	h2.mustRun("", "trust", own)
	if _, comments := h2.recipients(); comments[own] != own[len(own)-8:] {
		t.Fatalf("foreign key auto-named %q", comments[own])
	}
}

func parseRebuildFlags(t *testing.T, terminal Terminal, v rebuild.Verb, args ...string) (rebuild.Options, error) {
	t.Helper()
	app := New(proc.Streams{Stdin: strings.NewReader("y\n"), Stdout: io.Discard, Stderr: io.Discard}, terminal)
	var got rebuild.Options
	cmd := &cobra.Command{Use: v.String(), SilenceErrors: true, SilenceUsage: true}
	flags, err := addRebuildFlags(cmd, v)
	if err != nil {
		t.Fatal(err)
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		var err error
		got, err = app.rebuildOptions(flags, cmd, args)
		return err
	}
	cmd.SetArgs(args)
	return got, cmd.Execute()
}

func TestNixFlagsTranslate(t *testing.T) {
	got, err := parseRebuildFlags(t, Terminal{}, rebuild.Switch, "-L", "--show-trace", "-vv", "-j", "auto", "--cores", "4", "--option", "substitute=false", "--override-input", "nixpkgs=path:/x", "--install-bootloader", "--diff", "never", "--diff-full", "--", "--repair", "x")
	if err != nil {
		t.Fatal(err)
	}
	want := "--show-trace --print-build-logs -v -v --max-jobs auto --cores 4 --option substitute false --override-input nixpkgs path:/x --repair x"
	if strings.Join(got.Extra, " ") != want || !got.InstallBootloader || got.Confirm != nil {
		t.Fatalf("options = %+v\nwant extra %q", got, want)
	}
	if got.Diff != (rebuild.DiffOptions{Mode: rebuild.DiffNever, Full: true}) {
		t.Fatalf("diff options = %+v", got.Diff)
	}
	if got, err := parseRebuildFlags(t, Terminal{}, rebuild.Build); err != nil || got.Diff.Mode != rebuild.DiffAuto {
		t.Fatalf("default diff mode = %v, %v", got.Diff.Mode, err)
	}
	if _, err := parseRebuildFlags(t, Terminal{}, rebuild.Switch, "--diff", "sometimes"); err == nil {
		t.Fatal("--diff sometimes accepted")
	}
	if _, err := parseRebuildFlags(t, Terminal{}, rebuild.DryBuild, "--diff", "never"); err == nil {
		t.Fatal("dry-build offers --diff although it never diffs")
	}
	if _, err := parseRebuildFlags(t, Terminal{}, rebuild.Build, "--ask"); err == nil {
		t.Fatal("build offers --ask although it never activates")
	}
	if _, err := parseRebuildFlags(t, Terminal{}, rebuild.Switch, "--ask"); !usage.Is(err) {
		t.Fatalf("--ask without a terminal = %v, want a usage error", err)
	}
	asked, err := parseRebuildFlags(t, Terminal{StdinTTY: true}, rebuild.Switch, "--ask")
	if err != nil || asked.Confirm == nil {
		t.Fatalf("--ask on a terminal = %+v, %v", asked, err)
	}
	if ok, err := asked.Confirm("Activate?"); err != nil || !ok {
		t.Fatalf("confirm read y as %v, %v", ok, err)
	}
}

func TestConfirmAnswers(t *testing.T) {
	for answer, want := range map[string]bool{"y\n": true, "YES\n": true, "\n": false, "n\n": false, "": false, "yep\n": false} {
		var errOut bytes.Buffer
		app := New(proc.Streams{Stdin: strings.NewReader(answer), Stdout: io.Discard, Stderr: &errOut}, Terminal{StdinTTY: true})
		got, err := app.confirm("Activate?")
		if err != nil || got != want {
			t.Errorf("answer %q = %v, %v; want %v", answer, got, err, want)
		}
		if errOut.String() != "? Activate? [y/N] " {
			t.Errorf("prompt = %q", errOut.String())
		}
	}
}

func TestCompletion(t *testing.T) {
	h := newHarness(t)
	if out := h.mustRun("", "__complete", "switch", "--sh"); !strings.Contains(out, "--show-trace") {
		t.Fatalf("flag completion:\n%s", out)
	}
	if out := h.mustRun("", "__complete", "gc", "--d"); !strings.Contains(out, "--delete-older-than") {
		t.Fatalf("gc flag completion:\n%s", out)
	}
	for _, shell := range []string{"zsh", "bash", "fish"} {
		if out := h.mustRun("", "completion", shell); !strings.Contains(out, "__complete") {
			t.Errorf("%s completion script does not call back into phix", shell)
		}
	}

	profiles := filepath.Join(t.TempDir(), "profiles")
	if err := os.MkdirAll(profiles, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"1", "2", "3"} {
		if err := os.Symlink(h.dir, filepath.Join(profiles, "system-"+id+"-link")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("system-3-link", filepath.Join(profiles, "system")); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	app := New(proc.Streams{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut}, Terminal{})
	app.profile = func(config.Platform) string { return filepath.Join(profiles, "system") }
	if code := app.Run(context.Background(), []string{"__complete", "rollback", ""}); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	lines := strings.Split(out.String(), "\n")
	if !strings.HasPrefix(lines[0], "2\t") || !strings.HasPrefix(lines[1], "1\t") || strings.Contains(out.String(), "3\t") {
		t.Fatalf("rollback completion:\n%s", out.String())
	}

	out.Reset()
	if code := app.Run(context.Background(), []string{"__complete", "diff", "1", ""}); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if lines := strings.Split(out.String(), "\n"); !strings.HasPrefix(lines[0], "3\t") || !strings.HasPrefix(lines[2], "1\t") {
		t.Fatalf("diff completion:\n%s", out.String())
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"__complete", "diff", "1", "2", ""}); code != 0 || strings.Contains(out.String(), "\t") {
		t.Fatalf("diff completes a third argument:\n%s", out.String())
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"__complete", "switch", "--diff", ""}); code != 0 || !strings.HasPrefix(out.String(), "auto\nalways\nnever\n") {
		t.Fatalf("--diff completion:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"diff", "9"}); code != 1 || !strings.Contains(errOut.String(), "generation 9") {
		t.Fatalf("diff of a missing generation exited %d: %s", code, errOut.String())
	}
	if code := app.Run(context.Background(), []string{"diff", "1", "2", "3"}); code != 2 {
		t.Fatalf("diff with three arguments exited %d", code)
	}
}

func TestPostSwitchHook(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := filepath.Join(dir, "hook")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" \"$PHIX_FLAKE\" \"$PUSH_URL\" > " + record + "\necho pushing quietly\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newHarnessWith(t, []map[string]any{
		{"description": "Pushing the closure", "exec": script, "args": []string{"push", "--quiet", "team/main"}, "env": map[string]string{"PUSH_URL": "https://cache.example"}},
		{"description": "Needs a token", "exec": script, "secret_env": map[string]string{"PUSH_TOKEN": "push-token"}},
	})
	var out, errOut bytes.Buffer
	app := New(proc.Streams{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut}, Terminal{})
	rt, err := app.runtime()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.runHook(t.Context(), rt, rt.Config.PostSwitch[0], "/nix/store/gen", false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if want := "push --quiet team/main /nix/store/gen\n" + h.flake + "\nhttps://cache.example\n"; string(got) != want {
		t.Fatalf("hook saw:\n%s\nwant:\n%s", got, want)
	}
	if errOut.String() != "> Pushing the closure\n" {
		t.Fatalf("routine hook output not condensed: %q", errOut.String())
	}

	errOut.Reset()
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if err := app.runHook(t.Context(), rt, rt.Config.PostSwitch[1], "/nix/store/gen", false); err != nil {
		t.Fatal(err)
	}
	if want := "> Needs a token\n! skipped: secrets/push-token is missing, run phix secret set push-token\n"; errOut.String() != want {
		t.Fatalf("missing secret: %q, want %q", errOut.String(), want)
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("hook ran without its secret")
	}
}

func TestPostSwitchRunsTheActivatedGenerationsHooks(t *testing.T) {
	dir := t.TempDir()
	hook := func(name string) string {
		script := filepath.Join(dir, name)
		body := "#!/bin/sh\ntouch " + filepath.Join(dir, name+".ran") + "\n"
		if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return script
	}
	previous, activated := hook("previous"), hook("activated")
	newHarnessWith(t, []map[string]any{{"description": "Pushing", "exec": previous}})
	app := New(proc.Streams{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}, Terminal{})
	rt, err := app.runtime()
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := os.Getenv("PHIX_CONFIG")
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, bytes.ReplaceAll(cfg, []byte(previous), []byte(activated)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.postSwitch(t.Context(), rt, "/nix/store/gen", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(activated + ".ran"); err != nil {
		t.Errorf("the activated generation's hook did not run: %v", err)
	}
	if _, err := os.Stat(previous + ".ran"); err == nil {
		t.Error("the previous generation's hook ran")
	}
}

func TestTrustPostQuantumAndSSHRecipients(t *testing.T) {
	requireTools(t, "sops", "age", "age-keygen", "ssh-to-age", "ssh-keygen")
	h := newHarness(t)
	host, err := shortHostname()
	if err != nil {
		t.Fatal(err)
	}

	adminKey := filepath.Join(h.dir, "admin.key")
	command(t, "age-keygen", "-o", adminKey)
	h.mustRun("", "trust", "admin", command(t, "age-keygen", "-y", adminKey))
	h.mustRun("hunter2\n", "secret", "set", "token")
	t.Setenv("SOPS_AGE_KEY_FILE", adminKey)

	pqKey := filepath.Join(h.dir, "pq.key")
	command(t, "age-keygen", "-pq", "-o", pqKey)
	pq := command(t, "age-keygen", "-y", pqKey)
	h.mustRun("", "trust", "vault", pq)

	remoteKey := filepath.Join(h.dir, "remote")
	command(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "root@remote", "-f", remoteKey)
	remotePub := command(t, "cat", remoteKey+".pub")
	remote := strings.Join(strings.Fields(remotePub)[:2], " ")
	h.mustRun("", append([]string{"trust", "remote"}, strings.Fields(remotePub)...)...)

	ownKey := filepath.Join(h.home, ".ssh", "id_ed25519")
	command(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", ownKey)
	ownPub := command(t, "cat", ownKey+".pub")
	own := strings.Join(strings.Fields(ownPub)[:2], " ")
	h.mustRun("", "trust", ownPub)

	pipedKey := filepath.Join(h.dir, "piped")
	command(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "me@piped", "-f", pipedKey)
	pipedPub := command(t, "cat", pipedKey+".pub")
	piped := strings.Join(strings.Fields(pipedPub)[:2], " ")
	h.mustRun("piped "+pipedPub+"\n", "trust")

	_, comments := h.recipients()
	for recipient, want := range map[string]string{pq: "vault", remote: "remote", own: host, piped: "piped"} {
		if comments[recipient] != want {
			t.Errorf("recipient %q labelled %q, want %q", recipient, comments[recipient], want)
		}
	}

	token := filepath.Join(h.flake, "secrets", "token")
	t.Setenv("SOPS_AGE_KEY_FILE", pqKey)
	if plain := command(t, "sops", "--decrypt", "--input-type", "binary", "--output-type", "binary", token); plain != "hunter2" {
		t.Fatalf("secrets/token decrypts to %q for the post-quantum key", plain)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", "")
	t.Setenv("SOPS_AGE_SSH_PRIVATE_KEY_FILE", remoteKey)
	if plain := command(t, "sops", "--decrypt", "--input-type", "binary", "--output-type", "binary", token); plain != "hunter2" {
		t.Fatalf("secrets/token decrypts to %q for the ssh key", plain)
	}

	if _, _, code := h.run("", "trust", "x", "ssh-ed25519", "AAAAC3NzaC1lZDI1NTE5AAAAIMpnPU"); code != 2 {
		t.Fatalf("truncated ssh key exited %d, want 2", code)
	}
}
