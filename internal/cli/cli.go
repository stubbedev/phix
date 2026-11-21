package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/stubbedev/phix/internal/config"
	"github.com/stubbedev/phix/internal/proc"
	"github.com/stubbedev/phix/internal/rebuild"
	"github.com/stubbedev/phix/internal/runlock"
	"github.com/stubbedev/phix/internal/ui"
	"github.com/stubbedev/phix/internal/usage"
	"github.com/stubbedev/phix/internal/version"
)

const (
	groupRebuild  = "rebuild"
	groupMaintain = "maintain"
	groupExtra    = "extra"
)

var reservedNames = []string{"help", "completion"}

type Terminal struct {
	StdinTTY bool
	Stdout   ui.Style
	Stderr   ui.Style
	Screen   ui.Size
	Resize   <-chan ui.Size
}

type App struct {
	console  ui.Console
	terminal Terminal
	cfg      config.Config
	cfgPath  string
	cfgErr   error
	attr     string
	profile  func(config.Platform) string
}

func Main(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	streams := proc.Streams{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	terminal, err := detectTerminal(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "phix: %v\n", err)
		return 2
	}
	return New(streams, terminal).Run(ctx, args)
}

func detectTerminal(ctx context.Context) (Terminal, error) {
	icons, err := ui.DetectIcons(os.Getenv)
	if err != nil {
		return Terminal{}, err
	}
	t := Terminal{
		StdinTTY: term.IsTerminal(int(os.Stdin.Fd())),
		Stdout:   ui.NewStyle(ui.DetectColor(os.Stdout, os.Getenv), icons),
		Stderr:   ui.NewStyle(ui.DetectColor(os.Stderr, os.Getenv), icons),
	}
	if os.Getenv("TERM") == "dumb" || !term.IsTerminal(int(os.Stderr.Fd())) {
		return t, nil
	}
	if width, height, err := term.GetSize(int(os.Stderr.Fd())); err == nil {
		t.Screen = ui.Size{Width: width, Height: height}
		t.Resize = ui.WatchSize(ctx, os.Stderr)
	}
	return t, nil
}

func New(streams proc.Streams, terminal Terminal) *App {
	a := &App{
		console:  ui.Console{Streams: streams, OutStyle: terminal.Stdout, ErrStyle: terminal.Stderr},
		terminal: terminal,
		profile:  rebuild.Profile,
	}
	path, err := config.DefaultPath(os.Getenv)
	if err == nil {
		a.cfgPath = path
		a.cfg, err = config.Load(path, os.Getenv)
	}
	a.cfgErr = err
	return a
}

func (a *App) Run(ctx context.Context, args []string) int {
	root, err := a.root()
	if err == nil {
		root.SetArgs(args)
		err = root.ExecuteContext(ctx)
	}
	if err == nil {
		return 0
	}
	if silent, ok := errors.AsType[*silentExit](err); ok {
		return silent.code
	}
	if errors.Is(err, rebuild.ErrDeclined) {
		a.console.Log().Warn("Not activating, nothing was changed")
		return 1
	}
	a.console.Log().Error(err)
	return exitCode(err)
}

type silentExit struct {
	code int
}

func (e *silentExit) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func exitCode(err error) int {
	if usage.Is(err) {
		return 2
	}
	if code, ok := proc.ExitCode(err); ok && code > 0 {
		return code
	}
	return 1
}

func (a *App) config() (config.Config, error) {
	if a.cfgErr != nil {
		return config.Config{}, a.cfgErr
	}
	cfg := a.cfg
	if a.attr != "" {
		if err := config.ValidateAttr(a.attr); err != nil {
			return config.Config{}, usage.Wrap(fmt.Errorf("--attr: %w", err))
		}
		cfg.Attr = a.attr
	}
	return cfg, nil
}

func (a *App) runtime() (rebuild.Runtime, error) {
	cfg, err := a.config()
	if err != nil {
		return rebuild.Runtime{}, err
	}
	return rebuild.Runtime{Config: cfg, Console: a.console, Screen: a.terminal.Screen, Resize: a.terminal.Resize}, nil
}

func (a *App) root() (*cobra.Command, error) {
	root := &cobra.Command{
		Use:   config.Name,
		Short: "Build, activate and maintain this machine's flake configuration",
		Long: `phix builds and activates this machine's configuration from the flake checkout,
and maintains the flake: input updates, sops secrets and age recipients.

Rebuilds evaluate and build as you with nix build, show the closure diff
against the running generation, and only activation runs as root.

Environment:
  PHIX_CONFIG        config file (default: $XDG_CONFIG_HOME/phix/config.yaml)
  PHIX_FLAKE_DIR     flake directory, overriding the config
  PHIX_NIXOS_CONFIG  nixosConfigurations attribute, overriding the config
                     (--attr overrides both)
  NO_COLOR           disable colored output
  FORCE_COLOR        color output even when it is not a terminal
  NERD_FONTS         1 to use Nerd Font icons (needs a Nerd Font); the default
                     icons render the same in every terminal and font`,
		Version:       version.Version,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          a.rejectUnknown,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := writeHelp(a.console.Stderr, a.console.ErrStyle, cmd); err != nil {
				return err
			}
			return usage.Errorf("no command given")
		},
	}
	a.installHelp(root)
	root.SetIn(a.console.Stdin)
	root.SetOut(a.console.Stdout)
	root.SetErr(a.console.Stderr)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usage.Wrap(err) })
	root.PersistentFlags().StringVar(&a.attr, "attr", "", "configuration attribute to build, overriding the config and PHIX_NIXOS_CONFIG")
	if err := root.RegisterFlagCompletionFunc("attr", a.completeHostAttrs); err != nil {
		return nil, fmt.Errorf("registering --attr completion: %w", err)
	}
	root.AddGroup(
		&cobra.Group{ID: groupRebuild, Title: "Rebuild:"},
		&cobra.Group{ID: groupMaintain, Title: "Maintenance:"},
	)
	for _, v := range rebuild.Verbs() {
		cmd, err := a.rebuildCmd(v)
		if err != nil {
			return nil, err
		}
		root.AddCommand(exclusive(cmd))
	}
	upgrade, err := a.upgradeCmd()
	if err != nil {
		return nil, err
	}
	mcp, err := a.mcpCmd()
	if err != nil {
		return nil, err
	}
	root.AddCommand(
		a.replCmd(),
		exclusive(a.rollbackCmd()),
		a.generationsCmd(),
		a.diffCmd(),
		exclusive(a.gcCmd()),
		exclusive(a.updateCmd()),
		exclusive(upgrade),
		a.whoamiCmd(),
		mcp,
		exclusive(a.trustCmd()),
		exclusive(a.secretCmd()),
		a.serviceCmd(),
	)
	if a.cfgErr != nil || len(a.cfg.Commands) == 0 {
		return root, nil
	}
	taken := slices.Clone(reservedNames)
	for _, c := range root.Commands() {
		taken = append(taken, c.Name())
	}
	root.AddGroup(&cobra.Group{ID: groupExtra, Title: "Extra:"})
	for _, c := range a.cfg.Commands {
		if slices.Contains(taken, c.Name) {
			return nil, fmt.Errorf("config command %q collides with a built-in command", c.Name)
		}
		root.AddCommand(a.externalCmd(c))
	}
	return root, nil
}

func exclusive(cmd *cobra.Command) *cobra.Command {
	for _, sub := range cmd.Commands() {
		exclusive(sub)
	}
	run := cmd.RunE
	if run == nil {
		return cmd
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		dir, err := config.CacheDir()
		if err != nil {
			return err
		}
		lock, err := runlock.Acquire(dir)
		if err != nil {
			return err
		}
		return errors.Join(run(cmd, args), lock.Release())
	}
	return cmd
}

func (a *App) rejectUnknown(_ *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	if a.cfgErr != nil {
		return usage.Errorf("unknown command %q; try 'phix help' (extra commands unavailable: %v)", args[0], a.cfgErr)
	}
	return usage.Errorf("unknown command %q; try 'phix help'", args[0])
}

func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usage.Errorf("%s takes no arguments, got %q", cmd.CommandPath(), args)
	}
	return nil
}

func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return usage.Errorf("%s takes %d argument(s), got %d; usage: %s", cmd.CommandPath(), n, len(args), cmd.UseLine())
		}
		return nil
	}
}

func maxArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) > n {
			return usage.Errorf("%s takes at most %d argument(s), got %d; usage: %s", cmd.CommandPath(), n, len(args), cmd.UseLine())
		}
		return nil
	}
}
