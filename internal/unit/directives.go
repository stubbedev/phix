package unit

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var ignoredExact = map[string][]string{
	"Unit": {
		"Documentation", "SourcePath", "StopWhenUnneeded", "AllowIsolate", "IgnoreOnIsolate",
		"JobTimeoutSec", "JobRunningTimeoutSec", "CollectMode", "DefaultDependencies",
	},
	"Service": {
		"Slice", "Delegate", "BusName", "NotifyAccess", "WatchdogSec", "WatchdogSignal",
		"FileDescriptorStoreMax", "FileDescriptorStorePreserve", "OOMPolicy", "OOMScoreAdjust",
		"SyslogIdentifier", "SyslogFacility", "SyslogLevel", "SyslogLevelPrefix", "LogLevelMax",
		"LogExtraFields", "LogNamespace", "NoNewPrivileges", "LockPersonality",
		"MemoryDenyWriteExecute", "KeyringMode", "RemoveIPC", "DevicePolicy", "DeviceAllow",
		"ReadOnlyPaths", "ReadWritePaths", "InaccessiblePaths", "ExecPaths", "NoExecPaths",
		"TemporaryFileSystem", "BindPaths", "BindReadOnlyPaths", "AmbientCapabilities",
		"CapabilityBoundingSet", "SecureBits", "UtmpIdentifier", "UtmpMode", "TasksAccounting",
		"TasksMax", "Personality", "IgnoreSIGPIPE", "CPUSchedulingPolicy", "CPUSchedulingPriority",
		"CPUSchedulingResetOnFork", "CPUAffinity", "NUMAPolicy", "NUMAMask", "TimerSlackNSec",
		"MountFlags", "RuntimeDirectoryMode", "StateDirectoryMode", "CacheDirectoryMode",
		"LogsDirectoryMode", "ConfigurationDirectoryMode", "RuntimeDirectoryPreserve",
	},
	"Timer": {"AccuracySec", "WakeSystem", "OnClockChange", "OnTimezoneChange"},
	"Socket": {
		"Backlog", "PassCredentials", "PassSecurity", "PassPacketInfo", "KeepAlive", "KeepAliveTimeSec",
		"KeepAliveIntervalSec", "KeepAliveProbes", "NoDelay", "Priority", "ReceiveBuffer", "SendBuffer",
		"IPTOS", "IPTTL", "Mark", "ReusePort", "FreeBind", "Transparent", "Broadcast", "BindIPv6Only",
		"BindToDevice", "DeferAcceptSec", "TriggerLimitIntervalSec", "TriggerLimitBurst",
		"PollLimitIntervalSec", "PollLimitBurst", "Timestamping", "SocketUser", "SocketGroup",
		"SELinuxContextFromNet", "SmackLabel", "SmackLabelIPIn", "SmackLabelIPOut", "Writable",
		"FlushPending", "MaxConnections", "MaxConnectionsPerSource", "PipeSize", "MessageQueueMaxMessages",
		"MessageQueueMessageSize", "TimeoutSec",
	},
}

var ignoredPrefixes = map[string][]string{
	"Service": {
		"Protect", "Private", "Restrict", "Memory", "CPU", "IO", "Limit", "IPAddress", "IPAccounting",
		"IPIngress", "IPEgress", "ManagedOOM", "LogRateLimit", "SystemCall", "SocketBind", "Startup",
		"DefaultMemory",
	},
}

func isIgnored(section, key string) bool {
	if slices.Contains(ignoredExact[section], key) {
		return true
	}
	for _, p := range ignoredPrefixes[section] {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

type builder struct {
	u               *Unit
	spec            specifiers
	host            Host
	local           *time.Location
	timeoutStartSet bool
}

func newBuilder(name Name, host Host, local *time.Location) *builder {
	u := &Unit{Name: name, StartLimit: defaultStartLimit}
	switch name.Kind() {
	case Service:
		u.Service = &ServiceConfig{
			Type:         Simple,
			RestartDelay: 100 * time.Millisecond,
			TimeoutStart: 90 * time.Second,
			TimeoutStop:  90 * time.Second,
			KillSignal:   syscall.SIGTERM,
			ReloadSignal: syscall.SIGHUP,
			SendSIGKILL:  true,
			Stderr:       Output{Kind: OutputInherit},
		}
	case Timer:
		u.Timer = &TimerConfig{RemainAfterElapse: true, Unit: name.WithKind(Service)}
	case Socket:
		u.Socket = &SocketConfig{Service: name.WithKind(Service), SocketMode: 0o666, DirectoryMode: 0o755, FDName: name.String()}
	}
	return &builder{u: u, spec: specifiers{host: host, name: name}, host: host, local: local}
}

func (b *builder) apply(section, key, value string) error {
	if strings.HasPrefix(key, "X-") {
		if section == "Unit" && key == "X-SwitchMethod" {
			m, ok := switchMethodNames[value]
			if !ok {
				return fmt.Errorf("unknown switch method %q", value)
			}
			b.u.SwitchMethod = m
		}
		return nil
	}
	var handled bool
	var err error
	switch section {
	case "Unit":
		handled, err = b.applyUnit(key, value)
	case "Install":
		handled = true
	case "Service":
		if b.u.Service == nil {
			return fmt.Errorf("[Service] section in a %s unit", b.u.Name.Kind())
		}
		handled, err = b.applyService(key, value)
	case "Timer":
		if b.u.Timer == nil {
			return fmt.Errorf("[Timer] section in a %s unit", b.u.Name.Kind())
		}
		handled, err = b.applyTimer(key, value)
	case "Socket":
		if b.u.Socket == nil {
			return fmt.Errorf("[Socket] section in a %s unit", b.u.Name.Kind())
		}
		handled, err = b.applySocket(key, value)
	default:
		return fmt.Errorf("section [%s] is not supported", section)
	}
	if err != nil || handled {
		return err
	}
	if isIgnored(section, key) {
		id := section + "." + key
		if !slices.Contains(b.u.Ignored, id) {
			b.u.Ignored = append(b.u.Ignored, id)
		}
		return nil
	}
	return fmt.Errorf("%s= is not supported", key)
}

func (b *builder) applyUnit(key, value string) (bool, error) {
	u := b.u
	var err error
	switch key {
	case "Description":
		u.Description, err = b.spec.expand(value)
	case "Wants":
		err = b.names(&u.Deps.Wants, value)
	case "Requires":
		err = b.names(&u.Deps.Requires, value)
	case "Requisite":
		err = b.names(&u.Deps.Requisite, value)
	case "BindsTo":
		err = b.names(&u.Deps.BindsTo, value)
	case "PartOf":
		err = b.names(&u.Deps.PartOf, value)
	case "Conflicts":
		err = b.names(&u.Deps.Conflicts, value)
	case "After":
		err = b.names(&u.Deps.After, value)
	case "Before":
		err = b.names(&u.Deps.Before, value)
	case "OnFailure":
		err = b.names(&u.OnFailure, value)
	case "OnSuccess":
		err = b.names(&u.OnSuccess, value)
	case "RefuseManualStart":
		u.RefuseManualStart, err = parseBool(value)
	case "RefuseManualStop":
		u.RefuseManualStop, err = parseBool(value)
	case "StartLimitIntervalSec", "StartLimitInterval":
		u.StartLimit.Interval, err = parseTimeout(value)
	case "StartLimitBurst":
		u.StartLimit.Burst, err = parseNonNegative(value)
	case "FailureAction", "SuccessAction", "StartLimitAction":
		if value != "" && value != "none" {
			err = fmt.Errorf("%s=%s: only none is supported", key, value)
		}
	default:
		cond, isCond, cerr := b.condition(key, value)
		if !isCond {
			return false, nil
		}
		if cerr != nil {
			return true, cerr
		}
		switch {
		case value == "" && strings.HasPrefix(key, "Condition"):
			u.Conditions = nil
		case value == "":
			u.Asserts = nil
		case strings.HasPrefix(key, "Condition"):
			u.Conditions = append(u.Conditions, cond)
		default:
			u.Asserts = append(u.Asserts, cond)
		}
	}
	return true, err
}

func (b *builder) condition(key, value string) (Condition, bool, error) {
	var kindName string
	switch {
	case strings.HasPrefix(key, "Condition"):
		kindName = strings.TrimPrefix(key, "Condition")
	case strings.HasPrefix(key, "Assert"):
		kindName = strings.TrimPrefix(key, "Assert")
	default:
		return Condition{}, false, nil
	}
	kind, ok := conditionKindNames[kindName]
	if !ok {
		return Condition{}, true, fmt.Errorf("%s= is not supported", key)
	}
	if value == "" {
		return Condition{}, true, nil
	}
	c := Condition{Key: key, Kind: kind}
	if strings.HasPrefix(value, "|") {
		c.Triggered, value = true, value[1:]
	}
	if strings.HasPrefix(value, "!") {
		c.Negate, value = true, value[1:]
	}
	arg, err := b.spec.expand(value)
	if err != nil {
		return Condition{}, true, err
	}
	c.Arg = arg
	return c, true, nil
}

func (b *builder) applyService(key, value string) (bool, error) {
	s := b.u.Service
	var err error
	switch key {
	case "Type":
		t, ok := serviceTypeNames[value]
		if !ok {
			return true, fmt.Errorf("service type %q is not supported (supported: simple, exec, oneshot, notify, notify-reload, idle)", value)
		}
		s.Type = t
	case "ExecCondition":
		err = b.commands(&s.ExecCondition, value)
	case "ExecStartPre":
		err = b.commands(&s.ExecStartPre, value)
	case "ExecStart":
		err = b.commands(&s.ExecStart, value)
	case "ExecStartPost":
		err = b.commands(&s.ExecStartPost, value)
	case "ExecReload":
		err = b.commands(&s.ExecReload, value)
	case "ExecStop":
		err = b.commands(&s.ExecStop, value)
	case "ExecStopPost":
		err = b.commands(&s.ExecStopPost, value)
	case "RemainAfterExit":
		s.RemainAfterExit, err = parseBool(value)
	case "Restart":
		p, ok := restartPolicyNames[value]
		if !ok {
			return true, fmt.Errorf("%q is not a restart policy", value)
		}
		s.Restart = p
	case "RestartSec":
		s.RestartDelay, err = parseTimeout(value)
	case "RestartPreventExitStatus":
		err = parseExitSet(&s.RestartPreventExitStatus, value)
	case "RestartForceExitStatus":
		err = parseExitSet(&s.RestartForceExitStatus, value)
	case "SuccessExitStatus":
		err = parseExitSet(&s.SuccessExitStatus, value)
	case "TimeoutStartSec":
		s.TimeoutStart, err = parseTimeout(value)
		b.timeoutStartSet = true
	case "TimeoutStopSec":
		s.TimeoutStop, err = parseTimeout(value)
	case "TimeoutSec":
		s.TimeoutStart, err = parseTimeout(value)
		s.TimeoutStop = s.TimeoutStart
		b.timeoutStartSet = true
	case "KillSignal":
		s.KillSignal, err = parseSignal(value)
	case "ReloadSignal":
		s.ReloadSignal, err = parseSignal(value)
	case "SendSIGKILL":
		s.SendSIGKILL, err = parseBool(value)
	case "KillMode":
		m, ok := killModeNames[value]
		if !ok {
			return true, fmt.Errorf("KillMode=%s is not a kill mode", value)
		}
		s.KillMode = m
	case "Environment":
		if value == "" {
			s.Environment = nil
			break
		}
		var vars []Var
		vars, err = parseAssignments(value, b.spec)
		s.Environment = append(s.Environment, vars...)
	case "EnvironmentFile":
		err = b.environmentFile(value)
	case "UnsetEnvironment":
		if value == "" {
			s.UnsetEnvironment = nil
			break
		}
		var words []string
		words, err = splitWords(value)
		s.UnsetEnvironment = append(s.UnsetEnvironment, words...)
	case "PassEnvironment":
	case "WorkingDirectory":
		err = b.workingDirectory(value)
	case "StandardInput":
		if value != "null" {
			err = fmt.Errorf("StandardInput=%s: only null is supported", value)
		}
	case "StandardOutput":
		s.Stdout, err = b.output(value)
	case "StandardError":
		s.Stderr, err = b.output(value)
	case "Nice":
		s.Nice, err = parseRange(value, -20, 19)
	case "IOSchedulingClass":
		s.IOSchedulingClass, err = parseIOClass(value)
	case "IOSchedulingPriority":
		s.IOSchedulingPriority, err = parseRange(value, 0, 7)
	case "UMask":
		var mask UMask
		if mask, err = ParseUMask(value); err == nil {
			s.UMask = &mask
		}
	case "RuntimeDirectory":
		err = b.directories(&s.Directories.Runtime, value)
	case "StateDirectory":
		err = b.directories(&s.Directories.State, value)
	case "CacheDirectory":
		err = b.directories(&s.Directories.Cache, value)
	case "LogsDirectory":
		err = b.directories(&s.Directories.Logs, value)
	case "ConfigurationDirectory":
		err = b.directories(&s.Directories.Configuration, value)
	default:
		return false, nil
	}
	return true, err
}

func (b *builder) applyTimer(key, value string) (bool, error) {
	t := b.u.Timer
	var err error
	switch key {
	case "OnActiveSec":
		err = spans(&t.OnActive, value)
	case "OnBootSec":
		err = spans(&t.OnBoot, value)
	case "OnStartupSec":
		err = spans(&t.OnStartup, value)
	case "OnUnitActiveSec":
		err = spans(&t.OnUnitActive, value)
	case "OnUnitInactiveSec":
		err = spans(&t.OnUnitInactive, value)
	case "OnCalendar":
		if value == "" {
			t.OnCalendar = nil
			break
		}
		var c Calendar
		c, err = ParseCalendar(value, b.local)
		t.OnCalendar = append(t.OnCalendar, c)
	case "Persistent":
		t.Persistent, err = parseBool(value)
	case "RandomizedDelaySec":
		t.RandomizedDelay, err = parseTimeout(value)
	case "FixedRandomDelay":
		t.FixedRandomDelay, err = parseBool(value)
	case "RemainAfterElapse":
		t.RemainAfterElapse, err = parseBool(value)
	case "Unit":
		var expanded string
		if expanded, err = b.spec.expand(value); err == nil {
			t.Unit, err = ParseName(expanded)
		}
	default:
		return false, nil
	}
	return true, err
}

func (b *builder) applySocket(key, value string) (bool, error) {
	s := b.u.Socket
	var err error
	switch key {
	case "ListenStream":
		err = b.listen(ListenStream, value)
	case "ListenDatagram":
		err = b.listen(ListenDatagram, value)
	case "ListenSequentialPacket":
		err = b.listen(ListenSeqPacket, value)
	case "Accept":
		var accept bool
		if accept, err = parseBool(value); err == nil && accept {
			err = errors.New("one service instance per connection (Accept=yes) is not supported")
		}
	case "Service":
		var expanded string
		if expanded, err = b.spec.expand(value); err == nil {
			s.Service, err = ParseName(expanded)
		}
		if err == nil && s.Service.Kind() != Service {
			err = fmt.Errorf("%s is not a service unit", s.Service)
		}
	case "SocketMode":
		s.SocketMode, err = parseMode(value)
	case "DirectoryMode":
		s.DirectoryMode, err = parseMode(value)
	case "RemoveOnStop":
		s.RemoveOnStop, err = parseBool(value)
	case "FileDescriptorName":
		if value == "" || len(value) > 255 || strings.ContainsAny(value, ": \t\n") {
			err = fmt.Errorf("file descriptor name %q is not valid", value)
		}
		s.FDName = value
	default:
		return false, nil
	}
	return true, err
}

func (b *builder) listen(kind ListenKind, value string) error {
	s := b.u.Socket
	if value == "" {
		s.Listen = nil
		return nil
	}
	addr, err := b.spec.expand(value)
	if err != nil {
		return err
	}
	switch {
	case filepath.IsAbs(addr):
		s.Listen = append(s.Listen, Listen{Kind: kind, Unix: filepath.Clean(addr)})
		return nil
	case strings.HasPrefix(addr, "@"):
		return fmt.Errorf("abstract socket %q is not supported", addr)
	case kind == ListenSeqPacket:
		return fmt.Errorf("sequential packet socket %q must be a unix path", addr)
	}
	if port, perr := strconv.Atoi(addr); perr == nil {
		if port < 1 || port > 65535 {
			return fmt.Errorf("port %d is outside 1..65535", port)
		}
		addr = ":" + addr
	} else if _, _, serr := net.SplitHostPort(addr); serr != nil {
		return fmt.Errorf("listen address %q is neither an absolute path, a port nor host:port: %w", addr, serr)
	}
	s.Listen = append(s.Listen, Listen{Kind: kind, Address: addr})
	return nil
}

func parseMode(s string) (fs.FileMode, error) {
	m, err := strconv.ParseUint(s, 8, 12)
	if err != nil {
		return 0, fmt.Errorf("%q is not an octal file mode", s)
	}
	return fs.FileMode(m) & fs.ModePerm, nil
}

func (b *builder) names(dst *[]Name, value string) error {
	if value == "" {
		*dst = nil
		return nil
	}
	words, err := splitWords(value)
	if err != nil {
		return err
	}
	for _, w := range words {
		expanded, err := b.spec.expand(w)
		if err != nil {
			return err
		}
		n, err := ParseName(expanded)
		if err != nil {
			return err
		}
		if !slices.Contains(*dst, n) {
			*dst = append(*dst, n)
		}
	}
	return nil
}

func (b *builder) commands(dst *[]Command, value string) error {
	if value == "" {
		*dst = nil
		return nil
	}
	cmds, err := parseCommands(value, b.spec)
	if err != nil {
		return err
	}
	*dst = append(*dst, cmds...)
	return nil
}

func (b *builder) environmentFile(value string) error {
	s := b.u.Service
	if value == "" {
		s.EnvironmentFiles = nil
		return nil
	}
	f := EnvironmentFile{}
	if strings.HasPrefix(value, "-") {
		f.Optional, value = true, value[1:]
	}
	path, err := b.spec.expand(value)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("EnvironmentFile=%s is not an absolute path", path)
	}
	f.Path = path
	s.EnvironmentFiles = append(s.EnvironmentFiles, f)
	return nil
}

func (b *builder) workingDirectory(value string) error {
	s := b.u.Service
	s.WorkingDirectoryOptional = strings.HasPrefix(value, "-")
	value = strings.TrimPrefix(value, "-")
	switch value {
	case "":
		s.WorkingDirectory = ""
		return nil
	case "~":
		s.WorkingDirectory = b.host.Home
		return nil
	}
	path, err := b.spec.expand(value)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("WorkingDirectory=%s is not an absolute path", path)
	}
	s.WorkingDirectory = path
	return nil
}

func (b *builder) output(value string) (Output, error) {
	switch value {
	case "journal", "journal+console", "syslog", "syslog+console":
		return Output{Kind: OutputJournal}, nil
	case "inherit":
		return Output{Kind: OutputInherit}, nil
	case "null":
		return Output{Kind: OutputNull}, nil
	}
	kinds := []struct {
		prefix string
		kind   OutputKind
	}{{"file:", OutputFile}, {"append:", OutputAppend}, {"truncate:", OutputTruncate}}
	for _, k := range kinds {
		if raw, ok := strings.CutPrefix(value, k.prefix); ok {
			path, err := b.spec.expand(raw)
			if err != nil {
				return Output{}, err
			}
			if !filepath.IsAbs(path) {
				return Output{}, fmt.Errorf("output path %q is not absolute", path)
			}
			return Output{Kind: k.kind, Path: path}, nil
		}
	}
	return Output{}, fmt.Errorf("output %q is not supported (supported: journal, inherit, null, file:PATH, append:PATH, truncate:PATH)", value)
}

func (b *builder) directories(dst *[]string, value string) error {
	if value == "" {
		*dst = nil
		return nil
	}
	words, err := splitWords(value)
	if err != nil {
		return err
	}
	for _, w := range words {
		dir, err := b.spec.expand(w)
		if err != nil {
			return err
		}
		clean := filepath.Clean(dir)
		if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("directory %q must be a relative path inside its base", dir)
		}
		*dst = append(*dst, clean)
	}
	return nil
}

func spans(dst *[]time.Duration, value string) error {
	if value == "" {
		*dst = nil
		return nil
	}
	d, err := parseSpan(value)
	if err != nil {
		return err
	}
	*dst = append(*dst, d)
	return nil
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "1", "yes", "y", "true", "t", "on":
		return true, nil
	case "0", "no", "n", "false", "f", "off":
		return false, nil
	}
	return false, fmt.Errorf("%q is not a boolean", s)
}

func parseNonNegative(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not a non-negative integer", s)
	}
	return n, nil
}

func parseRange(s string, lo, hi int) (*int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi {
		return nil, fmt.Errorf("%q is not an integer in %d..%d", s, lo, hi)
	}
	return &n, nil
}

var ioClassNames = map[string]int{"none": 0, "realtime": 1, "best-effort": 2, "idle": 3}

func parseIOClass(s string) (*int, error) {
	if n, ok := ioClassNames[s]; ok {
		return &n, nil
	}
	return parseRange(s, 0, 3)
}

func parseSignal(s string) (syscall.Signal, error) {
	if n, err := strconv.Atoi(s); err == nil && n > 0 && n < 65 {
		return syscall.Signal(n), nil
	}
	name := strings.ToUpper(s)
	if !strings.HasPrefix(name, "SIG") {
		name = "SIG" + name
	}
	if sig := unix.SignalNum(name); sig != 0 {
		return sig, nil
	}
	return 0, fmt.Errorf("%q is not a signal", s)
}

var exitStatusNames = map[string]int{
	"SUCCESS": 0, "FAILURE": 1, "INVALIDARGUMENT": 2, "NOTIMPLEMENTED": 3, "NOPERMISSION": 4,
	"NOTINSTALLED": 5, "NOTCONFIGURED": 6, "NOTRUNNING": 7,
	"USAGE": 64, "DATAERR": 65, "NOINPUT": 66, "NOUSER": 67, "NOHOST": 68, "UNAVAILABLE": 69,
	"SOFTWARE": 70, "OSERR": 71, "OSFILE": 72, "CANTCREAT": 73, "IOERR": 74, "TEMPFAIL": 75,
	"PROTOCOL": 76, "NOPERM": 77, "CONFIG": 78,
}

func parseExitSet(dst *ExitSet, value string) error {
	if value == "" {
		*dst = ExitSet{}
		return nil
	}
	for w := range strings.FieldsSeq(value) {
		if n, ok := exitStatusNames[w]; ok {
			dst.Codes = append(dst.Codes, n)
			continue
		}
		if n, err := strconv.Atoi(w); err == nil {
			if n < 0 || n > 255 {
				return fmt.Errorf("exit status %d is outside 0..255", n)
			}
			dst.Codes = append(dst.Codes, n)
			continue
		}
		sig, err := parseSignal(w)
		if err != nil {
			return errors.New("exit status " + strconv.Quote(w) + " is neither a number, an exit status name nor a signal")
		}
		dst.Signals = append(dst.Signals, sig)
	}
	return nil
}

func (b *builder) finish() error {
	u := b.u
	if u.Name.Kind() == Target {
		for _, n := range slices.Concat(u.Deps.Wants, u.Deps.Requires) {
			if !slices.Contains(u.Deps.After, n) {
				u.Deps.After = append(u.Deps.After, n)
			}
		}
	}
	if s := u.Service; s != nil {
		switch {
		case s.Type == Oneshot && len(s.ExecStart) == 0 && len(s.ExecStop) == 0:
			return errors.New("a oneshot service needs ExecStart= or ExecStop=")
		case s.Type != Oneshot && len(s.ExecStart) != 1:
			return fmt.Errorf("a %s service needs exactly one ExecStart=, has %d", serviceTypeName(s.Type), len(s.ExecStart))
		case s.Type == Oneshot && (s.Restart == RestartAlways || s.Restart == RestartOnSuccess):
			return errors.New("oneshot services cannot use Restart=always or Restart=on-success")
		}
		if s.Type == Oneshot && !b.timeoutStartSet {
			s.TimeoutStart = 0
		}
	}
	if s := u.Socket; s != nil {
		if len(s.Listen) == 0 {
			return errors.New("a socket needs at least one Listen*= address")
		}
		if !slices.Contains(u.Deps.Before, s.Service) {
			u.Deps.Before = append(u.Deps.Before, s.Service)
		}
	}
	if t := u.Timer; t != nil {
		if len(t.OnActive)+len(t.OnBoot)+len(t.OnStartup)+len(t.OnUnitActive)+len(t.OnUnitInactive)+len(t.OnCalendar) == 0 {
			return errors.New("a timer needs at least one On*= trigger")
		}
		if t.Unit.Kind() == Timer {
			return errors.New("a timer cannot trigger another timer")
		}
	}
	return nil
}

func serviceTypeName(t ServiceType) string {
	for name, v := range serviceTypeNames {
		if v == t && name != "idle" {
			return name
		}
	}
	return fmt.Sprintf("ServiceType(%d)", int(t))
}
