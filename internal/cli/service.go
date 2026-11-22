package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/stubbedev/phix/internal/config"
	"github.com/stubbedev/phix/internal/supervisor"
	"github.com/stubbedev/phix/internal/ui"
	"github.com/stubbedev/phix/internal/unit"
	"github.com/stubbedev/phix/internal/usage"
)

const defaultLogLines = 10

func (a *App) serviceLayout() (supervisor.Layout, error) {
	cfg, err := a.config()
	if err != nil {
		return supervisor.Layout{}, err
	}
	if cfg.Platform != config.Android {
		return supervisor.Layout{}, fmt.Errorf("phix service runs user services where no systemd does; this %s host runs them under systemd, use systemctl --user", cfg.Platform)
	}
	return supervisor.DetectLayout(os.Getenv)
}

func (a *App) serviceProgram() (supervisor.Program, error) {
	cfg, err := a.config()
	if err != nil {
		return supervisor.Program{}, err
	}
	self, err := selfExecutable()
	if err != nil {
		return supervisor.Program{}, err
	}
	return supervisor.Program{Executable: self, ExecPath: cfg.ExecPath}, nil
}

func selfExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating the phix executable: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolving the phix executable %s: %w", path, err)
	}
	return resolved, nil
}

func (a *App) serviceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Run home-manager's systemd user units on Android, where no systemd runs",
		Long: `Run home-manager's systemd user units on Android, where no systemd runs.

A supervisor reads the units home-manager writes to ~/.config/systemd/user
(and ~/.local/share/systemd/user), starts everything default.target wants,
restarts services by their Restart= policy and fires timers. phix switch
restarts the units that changed, stops removed ones and starts new ones.

Services log to $XDG_STATE_HOME/phix/services/logs/<unit>.log. Directives
that have no effect without systemd (sandboxing, resource control) are
accepted and listed in the status; anything else unsupported keeps the unit
from loading, with the reason in the status.

With no subcommand, phix service prints the status of every unit.`,
		GroupID:           groupMaintain,
		Args:              noArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serviceStatus(cmd.Context(), nil, false)
		},
	}
	cmd.AddCommand(
		a.serviceStatusCmd(),
		a.serviceControlCmd(supervisor.OpStart, "Start units and what they require"),
		a.serviceControlCmd(supervisor.OpStop, "Stop units and the units bound to them"),
		a.serviceControlCmd(supervisor.OpRestart, "Restart units and the units part of them"),
		a.serviceControlCmd(supervisor.OpReload, "Reload units with ExecReload= or Type=notify-reload"),
		a.serviceLogsCmd(),
		a.serviceUpCmd(),
		a.serviceDownCmd(),
		a.serviceSyncCmd(),
		a.serviceSuperviseCmd(),
		a.serviceExecCmd(),
		a.systemctlCmd(),
		a.journalctlCmd(),
	)
	return cmd
}

func (a *App) serviceClient() (supervisor.Client, error) {
	layout, err := a.serviceLayout()
	if err != nil {
		return supervisor.Client{}, err
	}
	return supervisor.NewClient(layout), nil
}

func (a *App) completeUnits(_ *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	layout, err := a.serviceLayout()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []cobra.Completion
	for _, dir := range layout.Host.UnitDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			n, err := unit.ParseName(e.Name())
			if err != nil || n.IsTemplate() || !n.Kind().Supported() || slices.Contains(out, e.Name()) {
				continue
			}
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out, cobra.ShellCompDirectiveNoFileComp
}

func (a *App) serviceStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:               "status [unit...]",
		Short:             "Show every unit, or the given units in detail with their recent log",
		ValidArgsFunction: a.completeUnits,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceStatus(cmd.Context(), args, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the status as JSON")
	return cmd
}

func (a *App) serviceStatus(ctx context.Context, units []string, asJSON bool) error {
	layout, err := a.serviceLayout()
	if err != nil {
		return err
	}
	resp, err := supervisor.NewClient(layout).Call(ctx, supervisor.Request{Op: supervisor.OpStatus, Units: units})
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(a.console.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp.Status)
	}
	now := time.Now()
	if len(units) == 0 {
		return writeStatusTable(a.console.Stdout, a.console.OutStyle, resp.Status, now)
	}
	for i, s := range resp.Status {
		if i > 0 {
			fmt.Fprintln(a.console.Stdout)
		}
		lines, err := supervisor.ReadLog(layout, s.Name, defaultLogLines)
		if err != nil {
			return err
		}
		writeUnitDetail(a.console.Out(), s, lines, now)
	}
	return nil
}

func stateAttr(s supervisor.UnitStatus) ui.Attr {
	switch {
	case s.LoadError != "" || s.Active == "failed":
		return ui.Red
	case s.Active == "active":
		return ui.Green
	case s.Active == "inactive":
		return ui.Faint
	}
	return ui.Yellow
}

func stateText(s supervisor.UnitStatus) string {
	switch {
	case s.LoadError != "":
		return "not loaded"
	case s.Job != "":
		return s.Active + " (" + s.Sub + ", " + s.Job + " job)"
	}
	return s.Active + " (" + s.Sub + ")"
}

func writeStatusTable(w io.Writer, st ui.Style, statuses []supervisor.UnitStatus, now time.Time) error {
	t := ui.Table{Header: []string{"UNIT", "STATE", "SINCE", "DETAIL"}}
	for _, s := range statuses {
		since := ""
		if !s.Since.IsZero() && s.LoadError == "" {
			since = ui.Duration(now.Sub(s.Since)) + " ago"
		}
		t.Rows = append(t.Rows, []string{
			st.Paint(s.Name, ui.Bold),
			st.Paint(stateText(s), stateAttr(s)),
			since,
			unitDetail(st, s, now),
		})
	}
	return t.Render(w, st)
}

func unitDetail(st ui.Style, s supervisor.UnitStatus, now time.Time) string {
	switch {
	case s.LoadError != "":
		return st.Paint(s.LoadError, ui.Red)
	case !s.NextElapse.IsZero():
		return "next in " + ui.Duration(s.NextElapse.Sub(now)) + " → " + s.Triggers
	case len(s.Listen) > 0:
		return strings.Join(s.Listen, ", ") + " → " + s.Triggers
	case s.MainPID != 0:
		detail := "pid " + strconv.Itoa(s.MainPID)
		if s.StatusText != "" {
			detail += ", " + s.StatusText
		}
		return detail
	case s.Active == "failed":
		return st.Paint("result: "+s.Result, ui.Red)
	}
	return s.Description
}

func writeUnitDetail(p ui.Printer, s supervisor.UnitStatus, logs []string, now time.Time) {
	st := p.Style
	title := st.Paint(s.Name, ui.Bold)
	if s.Description != "" {
		title += " - " + s.Description
	}
	fmt.Fprintln(p.W, st.Paint("●", stateAttr(s))+" "+title)
	field := func(label, value string) {
		fmt.Fprintf(p.W, "%12s: %s\n", label, value)
	}
	if s.LoadError != "" {
		field("Loaded", st.Paint("error: "+s.LoadError, ui.Red))
		return
	}
	loaded := s.Fragment
	if loaded == "" {
		loaded = "builtin"
	}
	if s.Wanted {
		loaded += "; wanted by default.target"
	}
	field("Loaded", loaded)
	if len(s.Ignored) > 0 {
		field("Ignored", st.Paint(strings.Join(s.Ignored, ", "), ui.Yellow))
	}
	active := st.Paint(stateText(s), stateAttr(s))
	if !s.Since.IsZero() {
		active += " since " + s.Since.Format(time.RFC3339) + "; " + ui.Duration(now.Sub(s.Since)) + " ago"
	}
	field("Active", active)
	if s.Result != "" && s.Result != "success" {
		field("Result", s.Result)
	}
	if s.MainPID != 0 {
		field("Main PID", strconv.Itoa(s.MainPID))
	}
	if s.StatusText != "" {
		field("Status", strconv.Quote(s.StatusText))
	}
	if len(s.Listen) > 0 {
		field("Listen", strings.Join(s.Listen, ", "))
	}
	if s.Triggers != "" {
		trigger := s.Triggers
		if !s.NextElapse.IsZero() {
			trigger += "; next " + s.NextElapse.Format(time.RFC3339) + " (in " + ui.Duration(s.NextElapse.Sub(now)) + ")"
		}
		if !s.LastTrigger.IsZero() {
			trigger += "; last " + s.LastTrigger.Format(time.RFC3339)
		}
		field("Triggers", trigger)
	}
	if len(logs) > 0 {
		fmt.Fprintln(p.W)
		for _, l := range logs {
			fmt.Fprintln(p.W, st.Paint(l, ui.Faint))
		}
	}
}

func (a *App) serviceControlCmd(op supervisor.Op, short string) *cobra.Command {
	return &cobra.Command{
		Use:               string(op) + " <unit>...",
		Short:             short,
		ValidArgsFunction: a.completeUnits,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usage.Errorf("%s needs at least one unit; usage: %s", cmd.CommandPath(), cmd.UseLine())
			}
			client, err := a.serviceClient()
			if err != nil {
				return err
			}
			resp, err := client.Call(cmd.Context(), supervisor.Request{Op: op, Units: args})
			if err != nil {
				return err
			}
			return failuresError(op, resp.Failures)
		},
	}
}

func failuresError(op supervisor.Op, failures []string) error {
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("%s failed:\n  %s", op, strings.Join(failures, "\n  "))
}

func (a *App) serviceLogsCmd() *cobra.Command {
	var (
		follow bool
		lines  int
	)
	cmd := &cobra.Command{
		Use:               "logs [unit]",
		Short:             "Print a unit's log, or the supervisor's own without a unit; -f follows it",
		Args:              maxArgs(1),
		ValidArgsFunction: a.completeUnits,
		RunE: func(cmd *cobra.Command, args []string) error {
			if lines < 0 {
				return usage.Errorf("--lines must not be negative, got %d", lines)
			}
			layout, err := a.serviceLayout()
			if err != nil {
				return err
			}
			if !follow {
				name, err := supervisor.LogName(args)
				if err != nil {
					return err
				}
				n := lines
				if n == 0 {
					n = -1
				}
				tail, err := supervisor.ReadLog(layout, name, n)
				if err != nil {
					return err
				}
				for _, l := range tail {
					fmt.Fprintln(a.console.Stdout, l)
				}
				return nil
			}
			req := supervisor.Request{Op: supervisor.OpFollow, Units: args, Lines: lines}
			err = supervisor.NewClient(layout).Stream(cmd.Context(), req, func(r supervisor.Response) error {
				if r.Line != "" {
					fmt.Fprintln(a.console.Stdout, r.Line)
				}
				return nil
			})
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new lines as they are logged")
	cmd.Flags().IntVarP(&lines, "lines", "n", 0, "print only the last N lines (default: all)")
	return cmd
}

func (a *App) serviceUpCmd() *cobra.Command {
	var verbose bool
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start the supervisor unless it runs; quiet, for the login shell",
		Long: `Start the supervisor unless it already runs. The home-manager module runs this
from every login shell, since Android gives apps no boot hook: services come
up when the nix-on-droid app first opens a session.`,
		Args:              noArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			layout, err := a.serviceLayout()
			if err != nil {
				return err
			}
			program, err := a.serviceProgram()
			if err != nil {
				return err
			}
			started, err := supervisor.Up(cmd.Context(), layout, program)
			if err != nil {
				return err
			}
			if verbose {
				if started {
					a.console.Log().Done("started the phix service supervisor")
				} else {
					a.console.Log().Done("the phix service supervisor is already running")
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "say whether the supervisor was started")
	return cmd
}

func (a *App) serviceDownCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "down",
		Short:             "Stop every unit and the supervisor",
		Args:              noArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.serviceClient()
			if err != nil {
				return err
			}
			_, err = client.Call(cmd.Context(), supervisor.Request{Op: supervisor.OpShutdown})
			return err
		},
	}
}

func (a *App) serviceSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Apply the current units: restart changed, stop removed, start new",
		Long: `Re-read the unit files and apply them the way sd-switch does for systemd:
units whose files changed are restarted (or reloaded, or kept, by
X-SwitchMethod=), removed units are stopped and new units that default.target
wants are started. When phix itself changed, the supervisor restarts on the
new phix. Starts the supervisor if it is not running. The home-manager module
runs this on every activation.`,
		Args:              noArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serviceSync(cmd.Context())
		},
	}
}

func (a *App) serviceSync(ctx context.Context) error {
	layout, err := a.serviceLayout()
	if err != nil {
		return err
	}
	program, err := a.serviceProgram()
	if err != nil {
		return err
	}
	client := supervisor.NewClient(layout)
	resp, err := client.Call(ctx, supervisor.Request{Op: supervisor.OpSync, Executable: program.Executable, ExecPath: program.ExecPath})
	if errors.Is(err, supervisor.ErrNotRunning) {
		return a.bootSupervisor(ctx, layout, program)
	}
	if err != nil {
		return err
	}
	writeChanges(a.console.Stdout, resp.Changes)
	if resp.Restarting {
		if _, err := supervisor.Up(ctx, layout, program); err != nil {
			return err
		}
	}
	return failuresError(supervisor.OpSync, resp.Failures)
}

func (a *App) bootSupervisor(ctx context.Context, layout supervisor.Layout, program supervisor.Program) error {
	if _, err := supervisor.Up(ctx, layout, program); err != nil {
		return err
	}
	resp, err := supervisor.NewClient(layout).Call(ctx, supervisor.Request{Op: supervisor.OpStatus})
	if err != nil {
		return err
	}
	var changes []supervisor.Change
	for _, s := range resp.Status {
		if s.Wanted && s.Kind != unit.Target.String() {
			changes = append(changes, supervisor.Change{Action: supervisor.ActionStart, Unit: s.Name})
		}
	}
	writeChanges(a.console.Stdout, changes)
	return nil
}

func writeChanges(w io.Writer, changes []supervisor.Change) {
	for _, action := range []supervisor.Action{supervisor.ActionStop, supervisor.ActionRestart, supervisor.ActionReload, supervisor.ActionStart} {
		var units []string
		for _, c := range changes {
			if c.Action == action {
				units = append(units, c.Unit)
			}
		}
		if len(units) > 0 {
			fmt.Fprintf(w, "%s the following user units: %s\n", action, strings.Join(units, ", "))
		}
	}
}

func (a *App) serviceSuperviseCmd() *cobra.Command {
	var (
		readyFD  int
		execPath []string
	)
	cmd := &cobra.Command{
		Use:    "supervise",
		Short:  "Run the supervisor in the foreground",
		Hidden: true,
		Args:   noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			layout, err := supervisor.DetectLayout(os.Getenv)
			if err != nil {
				return err
			}
			self, err := selfExecutable()
			if err != nil {
				return err
			}
			ready := func(error) {}
			if readyFD > 0 {
				ready = supervisor.ReadyReporter(os.NewFile(uintptr(readyFD), "ready"))
			}
			return supervisor.Supervise(cmd.Context(), layout, supervisor.Program{Executable: self, ExecPath: execPath}, os.Environ(), ready)
		},
	}
	cmd.Flags().IntVar(&readyFD, "ready-fd", 0, "file descriptor to report readiness on")
	cmd.Flags().StringArrayVar(&execPath, "exec-path", nil, "directory searched for bare executable names after the unit's PATH")
	return cmd
}

func (a *App) serviceExecCmd() *cobra.Command {
	var nice, ioClass, ioPriority int
	var umask string
	var listenPID bool
	cmd := &cobra.Command{
		Use:    "exec [flags] -- <path> <argv0> [args...]",
		Short:  "Apply process attributes and exec a unit's command",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 {
				return usage.Errorf("%s needs a path and an argv[0]", cmd.CommandPath())
			}
			spec := supervisor.ExecSpec{ListenPID: listenPID}
			flags := cmd.Flags()
			if flags.Changed("nice") {
				spec.Nice = &nice
			}
			if flags.Changed("io-class") {
				spec.IOClass = &ioClass
			}
			if flags.Changed("io-priority") {
				spec.IOPriority = &ioPriority
			}
			if flags.Changed("umask") {
				mask, err := unit.ParseUMask(umask)
				if err != nil {
					return usage.Errorf("--umask: %w", err)
				}
				spec.UMask = &mask
			}
			return supervisor.Exec(spec, args[0], args[1:])
		},
	}
	cmd.Flags().IntVar(&nice, "nice", 0, "nice value")
	cmd.Flags().IntVar(&ioClass, "io-class", 0, "IO scheduling class")
	cmd.Flags().IntVar(&ioPriority, "io-priority", 0, "IO scheduling priority")
	cmd.Flags().StringVar(&umask, "umask", "", "octal umask")
	cmd.Flags().BoolVar(&listenPID, "listen-pid", false, "set LISTEN_PID to this process for socket activation")
	return cmd
}
