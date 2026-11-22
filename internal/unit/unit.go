package unit

import (
	"io/fs"
	"syscall"
	"time"
)

type Unit struct {
	Name              Name
	Description       string
	Fragment          string
	DropIns           []string
	Digest            string
	Deps              Deps
	Conditions        []Condition
	Asserts           []Condition
	StartLimit        StartLimit
	OnFailure         []Name
	OnSuccess         []Name
	RefuseManualStart bool
	RefuseManualStop  bool
	SwitchMethod      SwitchMethod
	Ignored           []string
	Service           *ServiceConfig
	Timer             *TimerConfig
	Socket            *SocketConfig
}

func (u *Unit) Label() string {
	if u.Description != "" {
		return u.Description
	}
	return u.Name.String()
}

type Deps struct {
	Wants     []Name
	Requires  []Name
	Requisite []Name
	BindsTo   []Name
	PartOf    []Name
	Conflicts []Name
	After     []Name
	Before    []Name
}

type StartLimit struct {
	Interval time.Duration
	Burst    int
}

var defaultStartLimit = StartLimit{Interval: 10 * time.Second, Burst: 5}

type SwitchMethod int

const (
	SwitchRestart SwitchMethod = iota
	SwitchReload
	SwitchStopStart
	SwitchKeepOld
)

var switchMethodNames = map[string]SwitchMethod{
	"restart":    SwitchRestart,
	"reload":     SwitchReload,
	"stop-start": SwitchStopStart,
	"keep-old":   SwitchKeepOld,
}

type ServiceType int

const (
	Simple ServiceType = iota + 1
	Exec
	Oneshot
	Notify
	NotifyReload
)

var serviceTypeNames = map[string]ServiceType{
	"simple":        Simple,
	"idle":          Simple,
	"exec":          Exec,
	"oneshot":       Oneshot,
	"notify":        Notify,
	"notify-reload": NotifyReload,
}

func (t ServiceType) Notifies() bool { return t == Notify || t == NotifyReload }

type RestartPolicy int

const (
	RestartNo RestartPolicy = iota
	RestartOnSuccess
	RestartOnFailure
	RestartOnAbnormal
	RestartOnAbort
	RestartOnWatchdog
	RestartAlways
)

var restartPolicyNames = map[string]RestartPolicy{
	"no":          RestartNo,
	"on-success":  RestartOnSuccess,
	"on-failure":  RestartOnFailure,
	"on-abnormal": RestartOnAbnormal,
	"on-abort":    RestartOnAbort,
	"on-watchdog": RestartOnWatchdog,
	"always":      RestartAlways,
}

type KillMode int

const (
	KillControlGroup KillMode = iota
	KillMixed
	KillProcess
	KillNone
)

var killModeNames = map[string]KillMode{
	"control-group": KillControlGroup,
	"mixed":         KillMixed,
	"process":       KillProcess,
	"none":          KillNone,
}

type OutputKind int

const (
	OutputJournal OutputKind = iota
	OutputInherit
	OutputNull
	OutputFile
	OutputAppend
	OutputTruncate
)

type Output struct {
	Kind OutputKind
	Path string
}

type ExitSet struct {
	Codes   []int
	Signals []syscall.Signal
}

type EnvironmentFile struct {
	Path     string
	Optional bool
}

type Directories struct {
	Runtime       []string
	State         []string
	Cache         []string
	Logs          []string
	Configuration []string
}

type ServiceConfig struct {
	Type                     ServiceType
	ExecCondition            []Command
	ExecStartPre             []Command
	ExecStart                []Command
	ExecStartPost            []Command
	ExecReload               []Command
	ExecStop                 []Command
	ExecStopPost             []Command
	RemainAfterExit          bool
	Restart                  RestartPolicy
	RestartDelay             time.Duration
	RestartPreventExitStatus ExitSet
	RestartForceExitStatus   ExitSet
	SuccessExitStatus        ExitSet
	TimeoutStart             time.Duration
	TimeoutStop              time.Duration
	KillSignal               syscall.Signal
	ReloadSignal             syscall.Signal
	SendSIGKILL              bool
	KillMode                 KillMode
	Environment              []Var
	EnvironmentFiles         []EnvironmentFile
	UnsetEnvironment         []string
	WorkingDirectory         string
	WorkingDirectoryOptional bool
	Stdout                   Output
	Stderr                   Output
	Nice                     *int
	IOSchedulingClass        *int
	IOSchedulingPriority     *int
	UMask                    *UMask
	Directories              Directories
}

type TimerConfig struct {
	OnActive          []time.Duration
	OnBoot            []time.Duration
	OnStartup         []time.Duration
	OnUnitActive      []time.Duration
	OnUnitInactive    []time.Duration
	OnCalendar        []Calendar
	Persistent        bool
	RandomizedDelay   time.Duration
	FixedRandomDelay  bool
	RemainAfterElapse bool
	Unit              Name
}

type ListenKind int

const (
	ListenStream ListenKind = iota + 1
	ListenDatagram
	ListenSeqPacket
)

type Listen struct {
	Kind    ListenKind
	Unix    string
	Address string
}

func (l Listen) Network() string {
	switch {
	case l.Unix != "" && l.Kind == ListenStream:
		return "unix"
	case l.Unix != "" && l.Kind == ListenDatagram:
		return "unixgram"
	case l.Unix != "":
		return "unixpacket"
	case l.Kind == ListenStream:
		return "tcp"
	}
	return "udp"
}

func (l Listen) String() string {
	if l.Unix != "" {
		return l.Unix
	}
	return l.Address
}

type SocketConfig struct {
	Listen        []Listen
	Service       Name
	SocketMode    fs.FileMode
	DirectoryMode fs.FileMode
	RemoveOnStop  bool
	FDName        string
}

type ConditionKind int

const (
	CondPathExists ConditionKind = iota + 1
	CondPathExistsGlob
	CondPathIsDirectory
	CondPathIsSymbolicLink
	CondPathIsReadWrite
	CondFileNotEmpty
	CondFileIsExecutable
	CondDirectoryNotEmpty
	CondEnvironment
	CondHost
	CondUser
)

var conditionKindNames = map[string]ConditionKind{
	"PathExists":         CondPathExists,
	"PathExistsGlob":     CondPathExistsGlob,
	"PathIsDirectory":    CondPathIsDirectory,
	"PathIsSymbolicLink": CondPathIsSymbolicLink,
	"PathIsReadWrite":    CondPathIsReadWrite,
	"FileNotEmpty":       CondFileNotEmpty,
	"FileIsExecutable":   CondFileIsExecutable,
	"DirectoryNotEmpty":  CondDirectoryNotEmpty,
	"Environment":        CondEnvironment,
	"Host":               CondHost,
	"User":               CondUser,
}

type Condition struct {
	Key       string
	Kind      ConditionKind
	Negate    bool
	Triggered bool
	Arg       string
}
