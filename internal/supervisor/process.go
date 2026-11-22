package supervisor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/stubbedev/phix/internal/unit"
)

type exitStatus struct {
	code     int
	signal   syscall.Signal
	spawnErr error
	timedOut bool
}

func (e exitStatus) String() string {
	switch {
	case e.spawnErr != nil:
		return "failed to start: " + e.spawnErr.Error()
	case e.timedOut:
		return "timed out"
	case e.signal != 0:
		return "killed by " + signalName(e.signal)
	}
	return "exited with status " + strconv.Itoa(e.code)
}

func statusOf(ps *os.ProcessState) exitStatus {
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if ok && ws.Signaled() {
		return exitStatus{signal: ws.Signal()}
	}
	return exitStatus{code: ps.ExitCode()}
}

func (e exitStatus) success(s *unit.ServiceConfig) bool {
	if e.spawnErr != nil || e.timedOut {
		return false
	}
	if e.signal != 0 {
		return slices.Contains(s.SuccessExitStatus.Signals, e.signal)
	}
	return e.code == 0 || slices.Contains(s.SuccessExitStatus.Codes, e.code)
}

var cleanSignals = []syscall.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGPIPE}

func (e exitStatus) clean(s *unit.ServiceConfig) bool {
	return e.success(s) || slices.Contains(cleanSignals, e.signal)
}

func (e exitStatus) result() string {
	switch {
	case e.spawnErr != nil:
		return "resources"
	case e.timedOut:
		return "timeout"
	case e.signal != 0:
		return "signal"
	case e.code != 0:
		return "exit-code"
	}
	return "success"
}

func (e exitStatus) in(set unit.ExitSet) bool {
	if e.signal != 0 {
		return slices.Contains(set.Signals, e.signal)
	}
	return e.spawnErr == nil && !e.timedOut && slices.Contains(set.Codes, e.code)
}

func shouldRestart(s *unit.ServiceConfig, e exitStatus) bool {
	switch {
	case e.in(s.RestartPreventExitStatus):
		return false
	case e.in(s.RestartForceExitStatus):
		return true
	}
	clean := e.clean(s)
	abnormal := e.timedOut || (e.signal != 0 && !clean)
	switch s.Restart {
	case unit.RestartAlways:
		return true
	case unit.RestartOnSuccess:
		return clean
	case unit.RestartOnFailure:
		return !clean
	case unit.RestartOnAbnormal:
		return abnormal
	case unit.RestartOnAbort:
		return e.signal != 0 && !clean
	}
	return false
}

func signalName(s syscall.Signal) string {
	if name := unix.SignalName(s); name != "" {
		return name
	}
	return "signal " + strconv.Itoa(int(s))
}

type process struct {
	pid  int
	role string
	cmd  *exec.Cmd
	done bool
}

func lookPath(name, pathEnv string, fallback []string) (string, error) {
	if filepath.IsAbs(name) {
		return name, executable(name)
	}
	for _, dir := range append(filepath.SplitList(pathEnv), fallback...) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, name)
		if executable(candidate) == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in PATH %q or the exec path %q", name, pathEnv, fallback)
}

func executable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return &fs.PathError{Op: "exec", Path: path, Err: errors.New("not an executable file")}
	}
	return nil
}

type attributes struct {
	nice      *int
	ioClass   *int
	ioPrio    *int
	umask     *unit.UMask
	listenPID bool
}

func (a attributes) any() bool {
	return a.nice != nil || a.ioClass != nil || a.ioPrio != nil || a.umask != nil || a.listenPID
}

func (a attributes) helperArgs() []string {
	var args []string
	if a.listenPID {
		args = append(args, "--listen-pid")
	}
	if a.nice != nil {
		args = append(args, "--nice", strconv.Itoa(*a.nice))
	}
	if a.ioClass != nil {
		args = append(args, "--io-class", strconv.Itoa(*a.ioClass))
	}
	if a.ioPrio != nil {
		args = append(args, "--io-priority", strconv.Itoa(*a.ioPrio))
	}
	if a.umask != nil {
		args = append(args, "--umask", a.umask.String())
	}
	return args
}

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func signalGroup(pgid int, sig syscall.Signal) error {
	err := syscall.Kill(-pgid, sig)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return fmt.Errorf("sending %s to process group %d: %w", signalName(sig), pgid, err)
}

func signalProcess(pid int, sig syscall.Signal) error {
	err := syscall.Kill(pid, sig)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return fmt.Errorf("sending %s to process %d: %w", signalName(sig), pid, err)
}
