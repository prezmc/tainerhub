// Package wire defines the messages exchanged between the TainerHub hub and
// its agents.
//
// Transport: each agent opens one long-lived, full-duplex HTTP/2 request to
// the hub's agent gateway (default port 7443), authenticated in both
// directions with TLS certificates issued by the hub's private CA. Both
// directions carry newline-delimited JSON: the request body carries
// AgentMessage values, the response body carries HubMessage values.
package wire

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const (
	PathEnroll  = "/v1/enroll"
	PathConnect = "/v1/connect"

	// ProtocolVersion is sent in Hello; bump on incompatible changes.
	ProtocolVersion = 1
)

// EnrollRequest is sent once, with the one-time join token, to obtain a
// client certificate. The TLS connection for this call has no client cert.
type EnrollRequest struct {
	Token    string `json:"token"`
	Hostname string `json:"hostname"`
	CSR      []byte `json:"csr"` // DER-encoded PKCS#10
}

type EnrollResponse struct {
	HostID string `json:"host_id"`
	Cert   []byte `json:"cert"` // DER, client certificate
	CA     []byte `json:"ca"`   // DER, hub CA certificate
}

// AgentMessage flows agent -> hub. Exactly one field is set.
type AgentMessage struct {
	Hello    *Hello         `json:"hello,omitempty"`
	Snapshot *Snapshot      `json:"snapshot,omitempty"`
	Event    *DockerEvent   `json:"event,omitempty"`
	Result   *CommandResult `json:"result,omitempty"`
	Log      *LogChunk      `json:"log,omitempty"`
	Ping     bool           `json:"ping,omitempty"`
}

// HubMessage flows hub -> agent. Exactly one field is set.
type HubMessage struct {
	Command *Command `json:"command,omitempty"`
	Ping    bool     `json:"ping,omitempty"`
	// Watching says whether anyone has TainerHub open. Agents sample CPU and
	// memory often while someone is watching and rarely otherwise, because
	// every sample costs the Docker Engine work for each container.
	Watching *bool `json:"watching,omitempty"`
	// StatsFromDocker makes the agent ask Docker for container stats even
	// when it can read cgroup files (an admin's choice, e.g. to compare).
	// Sent at connect and when it changes.
	StatsFromDocker *bool `json:"stats_from_docker,omitempty"`
}

type Hello struct {
	Protocol      int    `json:"protocol"`
	AgentVersion  string `json:"agent_version"`
	Hostname      string `json:"hostname"`
	DockerVersion string `json:"docker_version"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	NCPU          int    `json:"ncpu"`
	MemTotal      int64  `json:"mem_total"`

	// Stacks says whether this agent can run compose stacks. Nil from agents
	// older than stack support.
	Stacks *StackSupport `json:"stacks,omitempty"`
	// CgroupStats says whether the agent reads container stats from the
	// host's cgroup files (cheap) rather than the Docker API. Nil from older agents.
	CgroupStats *bool `json:"cgroup_stats,omitempty"`
}

// StackSupport reports the agent's compose setup. Error is set (and stacks
// are unavailable) when something is missing, e.g. the stacks folder mount.
type StackSupport struct {
	Dir            string `json:"dir"`
	ComposeVersion string `json:"compose_version,omitempty"`
	Error          string `json:"error,omitempty"`
}

type Container struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	State   string            `json:"state"`  // running, exited, paused, restarting, created, dead
	Status  string            `json:"status"` // human text, e.g. "Up 3 hours (healthy)"
	Created int64             `json:"created"`
	Project string            `json:"project,omitempty"` // compose project label
	Ports   []string          `json:"ports,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`

	// Live resource use, refreshed about every 10 s for running containers.
	// CPU is a percentage of one core (like `docker stats`), so it can exceed 100.
	CPU      float64 `json:"cpu,omitempty"`
	Mem      int64   `json:"mem,omitempty"`       // bytes, excluding page cache
	MemLimit int64   `json:"mem_limit,omitempty"` // bytes

	// UpdateAvailable: the registry has a newer image for this container's
	// tag (checked every few hours; see OpImagesCheckUpdates).
	UpdateAvailable bool `json:"update,omitempty"`
}

type Snapshot struct {
	At         int64       `json:"at"` // unix seconds
	Containers []Container `json:"containers"`
}

type DockerEvent struct {
	Type   string `json:"type"`
	Action string `json:"action"`
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Time   int64  `json:"time"`
}

// Command is an instruction from the hub. ID correlates results and logs.
// Exactly one of Action, Op, Logs, Stack or Cancel is set.
type Command struct {
	ID     string           `json:"id"`
	Action *ContainerAction `json:"action,omitempty"`
	Op     *Operation       `json:"op,omitempty"`
	Logs   *LogsRequest     `json:"logs,omitempty"`
	// Stack runs a compose operation. Its output streams back as LogChunks
	// with this command's ID; the final chunk (EOF) carries any error.
	Stack *StackCommand `json:"stack,omitempty"`
	// Exec opens an interactive terminal in a container. Its output streams
	// back as LogChunks with this command's ID; ExecInput and ExecResize
	// address it by that ID, and Cancel ends it.
	Exec       *ExecStart  `json:"exec,omitempty"`
	ExecInput  *ExecInput  `json:"exec_input,omitempty"`
	ExecResize *ExecResize `json:"exec_resize,omitempty"`
	Cancel     string      `json:"cancel,omitempty"` // ID of a running command to stop
}

// ---------------- exec (terminal) ----------------

type ExecStart struct {
	ContainerID string   `json:"container_id"`
	Cmd         []string `json:"cmd"` // empty: bash if the container has it, else sh
	Cols        int      `json:"cols"`
	Rows        int      `json:"rows"`
}

type ExecInput struct {
	Session string `json:"session"`
	Data    []byte `json:"data"`
}

type ExecResize struct {
	Session string `json:"session"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
}

// DefaultShell runs bash where the image has it, otherwise sh.
var DefaultShell = []string{"/bin/sh", "-c", "if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi"}

func ValidExecStart(e ExecStart) error {
	if !ValidContainerRef(e.ContainerID) {
		return errors.New("invalid container")
	}
	if len(e.Cmd) > 32 {
		return errors.New("command has too many arguments")
	}
	for _, a := range e.Cmd {
		if len(a) > 1024 || strings.ContainsRune(a, 0) {
			return errors.New("command argument too long or invalid")
		}
	}
	if !validTermSize(e.Cols, e.Rows) {
		return errors.New("invalid terminal size")
	}
	return nil
}

func validTermSize(cols, rows int) bool {
	return cols >= 1 && cols <= 1000 && rows >= 1 && rows <= 1000
}

func ValidExecResize(r ExecResize) bool { return validTermSize(r.Cols, r.Rows) }

// ---------------- compose stacks ----------------

// Stack actions.
const (
	StackDeploy  = "deploy"  // write the files, then `up -d --remove-orphans`
	StackStart   = "start"   // start existing containers
	StackStop    = "stop"    // stop containers, keep them
	StackRestart = "restart" // restart containers
	StackDown    = "down"    // remove containers and networks (volumes only if Volumes)
	StackRemove  = "remove"  // down, then delete the stack's files on the host
)

// Size limits for a stack's files.
const (
	MaxComposeBytes = 256 << 10
	MaxEnvBytes     = 64 << 10
)

type StackCommand struct {
	Action  string `json:"action"`
	Name    string `json:"name"`              // compose project name
	Compose string `json:"compose,omitempty"` // deploy only
	Env     string `json:"env,omitempty"`     // deploy only: contents of .env
	Pull    bool   `json:"pull,omitempty"`    // deploy: pull newer images first
	Volumes bool   `json:"volumes,omitempty"` // down / remove: also delete named volumes
}

// ValidStackName accepts compose project names: lowercase letters, digits,
// '-' and '_', starting with a letter or digit. The name is also the stack's
// folder name on the host, so it can never contain '/' or '..'.
func ValidStackName(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || (i > 0 && (c == '-' || c == '_'))) {
			return false
		}
	}
	return true
}

func ValidStackCommand(c StackCommand) error {
	switch c.Action {
	case StackDeploy, StackStart, StackStop, StackRestart, StackDown, StackRemove:
	default:
		return errors.New("unknown stack action")
	}
	if !ValidStackName(c.Name) {
		return errors.New("invalid stack name: use lowercase letters, digits, '-' and '_' (max 63)")
	}
	if c.Action == StackDeploy && strings.TrimSpace(c.Compose) == "" {
		return errors.New("compose file is empty")
	}
	if len(c.Compose) > MaxComposeBytes {
		return errors.New("compose file is too large (max 256 KB)")
	}
	if len(c.Env) > MaxEnvBytes {
		return errors.New(".env is too large (max 64 KB)")
	}
	return nil
}

// Valid container actions.
const (
	ActionStart   = "start"
	ActionStop    = "stop"
	ActionRestart = "restart"
	ActionPause   = "pause"
	ActionUnpause = "unpause"
	ActionKill    = "kill"
	ActionRemove  = "remove"
)

// ValidContainerRef accepts Docker container IDs and names only, so nothing
// that reaches the Docker API path can contain '/', '..', '?' or '%'.
func ValidContainerRef(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || (i > 0 && (c == '_' || c == '.' || c == '-'))
		if !ok {
			return false
		}
	}
	return true
}

// MaxMessageBytes bounds a single stream message in either direction.
const MaxMessageBytes = 8 << 20

// Decoder reads newline-delimited JSON with a per-message size limit, so a
// misbehaving peer cannot make the other side buffer unbounded data.
type Decoder struct{ s *bufio.Scanner }

func NewDecoder(r io.Reader) *Decoder {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64<<10), MaxMessageBytes)
	return &Decoder{s: s}
}

func (d *Decoder) Decode(v any) error {
	for d.s.Scan() {
		line := d.s.Bytes()
		if len(line) == 0 {
			continue
		}
		return json.Unmarshal(line, v)
	}
	if err := d.s.Err(); err != nil {
		return err
	}
	return io.EOF
}

func ValidAction(a string) bool {
	switch a {
	case ActionStart, ActionStop, ActionRestart, ActionPause, ActionUnpause, ActionKill, ActionRemove:
		return true
	}
	return false
}

type ContainerAction struct {
	ContainerID string `json:"container_id"`
	Action      string `json:"action"`
}

type LogsRequest struct {
	ContainerID string `json:"container_id"`
	Tail        int    `json:"tail"`
	Follow      bool   `json:"follow"`
}

type CommandResult struct {
	CommandID string          `json:"command_id"`
	OK        bool            `json:"ok"`
	Error     string          `json:"error,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

type LogChunk struct {
	CommandID string `json:"command_id"`
	Data      []byte `json:"data,omitempty"`
	EOF       bool   `json:"eof,omitempty"`
	Error     string `json:"error,omitempty"`
}

// JoinToken is the one-time credential an admin pastes into an agent's
// config. It carries the hub CA fingerprint so the agent can pin the hub
// before it has a certificate of its own.
type JoinToken struct {
	ID            string `json:"i"`
	Secret        string `json:"s"`
	CAFingerprint string `json:"f"` // hex SHA-256 of the CA certificate DER
}

const joinTokenPrefix = "thj1_"

func (t JoinToken) Encode() string {
	b, _ := json.Marshal(t)
	return joinTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
}

func ParseJoinToken(s string) (JoinToken, error) {
	var t JoinToken
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, joinTokenPrefix) {
		return t, errors.New("join token: wrong format")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, joinTokenPrefix))
	if err != nil {
		return t, errors.New("join token: bad encoding")
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return t, errors.New("join token: bad payload")
	}
	if t.ID == "" || t.Secret == "" || len(t.CAFingerprint) != 64 {
		return t, errors.New("join token: missing fields")
	}
	return t, nil
}

// ---------------- resource operations ----------------

// Operation is a whitelisted Docker request other than a container action:
// inspecting a container, or listing / removing / pruning images, volumes
// and networks. Agents refuse any Kind not listed in OpKinds.
type Operation struct {
	Kind   string `json:"kind"`
	Target string `json:"target,omitempty"` // container / image / volume / network ID or name
	Arg    string `json:"arg,omitempty"`    // image reference for pulls, "all" for prunes
}

const (
	OpContainerInspect = "container.inspect"
	OpImagesList       = "images.list"
	OpImagePull        = "image.pull"
	OpImageRemove      = "image.remove"
	OpImagesPrune      = "images.prune"
	OpVolumesList      = "volumes.list"
	OpVolumeRemove     = "volume.remove"
	OpVolumesPrune     = "volumes.prune"
	OpNetworksList     = "networks.list"
	OpNetworkRemove    = "network.remove"
	OpNetworksPrune    = "networks.prune"
	// OpContainerRemoveVolumes removes a container (even if running) with its
	// anonymous volumes, then deletes the named volumes listed in Arg
	// (comma-separated) that it mounted and nothing else uses.
	OpContainerRemoveVolumes = "container.remove_volumes"
	// OpSelfUpdate updates the Docker Compose project the agent itself runs
	// in (pull newer images, recreate what changed). On the hub's server that
	// project is the whole hub. Arg SelfUpdateAddCgroupMount instead adds the
	// read-only cgroup mount to the agent (in compose.override.yml next to the
	// project's compose file) and recreates just the agent, without pulling.
	OpSelfUpdate = "agent.update"
	// SelfUpdateAddCgroupMount is OpSelfUpdate's Arg for adding the mount.
	SelfUpdateAddCgroupMount = "add-cgroup-mount"
	// OpImagesCheckUpdates asks the registry, now, whether each container's
	// image tag has a newer image. Result: ImageUpdates.
	OpImagesCheckUpdates = "images.check_updates"
	// OpContainerRecreate pulls the container's image and recreates it with
	// the same settings, rolling back if the new one doesn't start. Standalone
	// containers only: compose ones are updated through their stack.
	OpContainerRecreate = "container.recreate"
	// OpDockerPlan inspects how Docker is installed on the host and what
	// version its package repository offers. Result: DockerPlan.
	OpDockerPlan = "docker.plan"
	// OpDockerUpgrade upgrades the Docker Engine packages on the host. Docker
	// restarts, and with it every container. Arg must be "restart-all" (the
	// admin's explicit OK). Result: DockerPlan of what was started.
	OpDockerUpgrade = "docker.upgrade"
)

// DockerPlan describes a host's Docker install and whether TainerHub can
// upgrade it.
type DockerPlan struct {
	Manager   string `json:"manager"`             // apt, dnf, or why not: snap, apt-docker.io, rpm-distro, unknown
	Installed string `json:"installed,omitempty"` // package version, e.g. 5:29.8.2-1~ubuntu.24.04~noble
	Candidate string `json:"candidate,omitempty"` // newest version the host's repository offers
	Systemd   bool   `json:"systemd"`
	Supported bool   `json:"supported"`        // TainerHub can run the upgrade
	Available bool   `json:"available"`        // the repository has a newer version
	Reason    string `json:"reason,omitempty"` // why not supported, in plain words
	Command   string `json:"command"`          // what runs (or what to run by hand)
}

// ImageUpdates maps image references (as containers use them) to whether a
// newer image is available.
type ImageUpdates struct {
	Checked int64             `json:"checked"` // unix seconds
	Images  map[string]bool   `json:"images"`
	Errors  map[string]string `json:"errors,omitempty"`
}

type RecreateResult struct {
	Status   string `json:"status"` // "updated" or "current"
	OldImage string `json:"oldImage"`
	NewImage string `json:"newImage"`
}

// SelfUpdateResult says what the agent started.
type SelfUpdateResult struct {
	Project string `json:"project"` // compose project being updated
	Dir     string `json:"dir"`     // its folder on the host
	Helper  string `json:"helper"`  // name of the short-lived container doing it
}

// MaxRemoveVolumes caps how many named volumes one removal may delete.
const MaxRemoveVolumes = 32

// OpKinds lists every allowed operation and whether it changes state.
var OpKinds = map[string]bool{
	OpContainerInspect: false, OpImagesList: false, OpVolumesList: false, OpNetworksList: false,
	OpImagePull: true, OpImageRemove: true, OpImagesPrune: true,
	OpVolumeRemove: true, OpVolumesPrune: true, OpNetworkRemove: true, OpNetworksPrune: true,
	OpContainerRemoveVolumes: true, OpSelfUpdate: true,
	OpImagesCheckUpdates: false, OpContainerRecreate: true,
	OpDockerPlan: false, OpDockerUpgrade: true,
}

// ValidOperation checks the kind and the shape of its target / argument.
func ValidOperation(op Operation) error {
	if _, ok := OpKinds[op.Kind]; !ok {
		return errors.New("unknown operation")
	}
	switch op.Kind {
	case OpContainerInspect, OpVolumeRemove, OpNetworkRemove, OpContainerRecreate:
		if !ValidContainerRef(op.Target) {
			return errors.New("invalid target")
		}
	case OpImageRemove:
		if !ValidImageID(op.Target) {
			return errors.New("invalid image id")
		}
	case OpImagePull:
		if !ValidImageRef(op.Arg) {
			return errors.New("invalid image reference")
		}
	case OpContainerRemoveVolumes:
		if !ValidContainerRef(op.Target) {
			return errors.New("invalid container")
		}
		if _, err := RemoveVolumeNames(op.Arg); err != nil {
			return err
		}
	case OpSelfUpdate:
		if op.Arg != "" && op.Arg != SelfUpdateAddCgroupMount {
			return errors.New("invalid update mode")
		}
	case OpDockerUpgrade:
		if op.Arg != "restart-all" {
			return errors.New("upgrading Docker restarts every container: it needs an explicit OK")
		}
	case OpImagesPrune, OpVolumesPrune, OpNetworksPrune:
		if op.Arg != "" && op.Arg != "all" {
			return errors.New("invalid prune mode")
		}
	}
	return nil
}

// RemoveVolumeNames splits and checks the Arg of OpContainerRemoveVolumes.
func RemoveVolumeNames(arg string) ([]string, error) {
	if arg == "" {
		return nil, nil
	}
	names := strings.Split(arg, ",")
	if len(names) > MaxRemoveVolumes {
		return nil, errors.New("too many volumes")
	}
	for _, n := range names {
		if !ValidContainerRef(n) {
			return nil, errors.New("invalid volume name")
		}
	}
	return names, nil
}

// ValidImageID accepts "sha256:<hex>" or bare hex IDs.
func ValidImageID(s string) bool {
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) < 12 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f') {
			return false
		}
	}
	return true
}

// ValidImageRef accepts references such as "nginx", "nginx:1.27",
// "ghcr.io/org/app:tag" or "repo@sha256:...".
func ValidImageRef(s string) bool {
	if len(s) == 0 || len(s) > 255 || strings.Contains(s, "..") || strings.Contains(s, "//") {
		return false
	}
	if c := s[0]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._-/:@", c) >= 0) {
			return false
		}
	}
	return true
}

// ---- operation result payloads ----

type ContainerDetail struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Image         string            `json:"image"`
	ImageID       string            `json:"imageId"`
	Created       string            `json:"created"`
	State         DetailState       `json:"state"`
	Cmd           []string          `json:"cmd,omitempty"`
	Entrypoint    []string          `json:"entrypoint,omitempty"`
	WorkingDir    string            `json:"workingDir,omitempty"`
	User          string            `json:"user,omitempty"`
	Env           []string          `json:"env,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	RestartPolicy string            `json:"restartPolicy"`
	MaxRetries    int               `json:"maxRetries,omitempty"`
	Privileged    bool              `json:"privileged"`
	NetworkMode   string            `json:"networkMode"`
	MemoryLimit   int64             `json:"memoryLimit,omitempty"`
	NanoCPUs      int64             `json:"nanoCpus,omitempty"`
	Mounts        []DetailMount     `json:"mounts"`
	Networks      []DetailNetwork   `json:"networks"`
	Ports         []DetailPort      `json:"ports"`
}

type DetailState struct {
	Status     string        `json:"status"`
	StartedAt  string        `json:"startedAt"`
	FinishedAt string        `json:"finishedAt"`
	ExitCode   int           `json:"exitCode"`
	OOMKilled  bool          `json:"oomKilled"`
	Error      string        `json:"error,omitempty"`
	Health     *DetailHealth `json:"health,omitempty"`
}

type DetailHealth struct {
	Status        string        `json:"status"`
	FailingStreak int           `json:"failingStreak"`
	Log           []HealthProbe `json:"log"`
}

type HealthProbe struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	ExitCode int    `json:"exitCode"`
	Output   string `json:"output"`
}

type DetailMount struct {
	Type        string `json:"type"`
	Name        string `json:"name,omitempty"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	RW          bool   `json:"rw"`
}

type DetailNetwork struct {
	Name    string `json:"name"`
	IP      string `json:"ip,omitempty"`
	Gateway string `json:"gateway,omitempty"`
}

type DetailPort struct {
	Container string `json:"container"` // e.g. "80/tcp"
	Host      string `json:"host,omitempty"`
}

type Image struct {
	ID      string   `json:"id"`
	Tags    []string `json:"tags"`
	Size    int64    `json:"size"`
	Created int64    `json:"created"`
	UsedBy  []string `json:"usedBy"`
}

type Volume struct {
	Name       string   `json:"name"`
	Driver     string   `json:"driver"`
	Mountpoint string   `json:"mountpoint"`
	Created    string   `json:"created"`
	Project    string   `json:"project,omitempty"`
	UsedBy     []string `json:"usedBy"`
}

type Network struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Driver   string   `json:"driver"`
	Scope    string   `json:"scope"`
	Internal bool     `json:"internal"`
	Builtin  bool     `json:"builtin"`
	Project  string   `json:"project,omitempty"`
	UsedBy   []string `json:"usedBy"`
}

// RemoveVolumesResult reports what OpContainerRemoveVolumes did.
type RemoveVolumesResult struct {
	Anonymous int          `json:"anonymous"` // anonymous volumes removed with the container
	Removed   []string     `json:"removed"`   // named volumes deleted
	Kept      []KeptVolume `json:"kept"`      // named volumes left alone, and why
}

type KeptVolume struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type PruneResult struct {
	Deleted   int   `json:"deleted"`
	Reclaimed int64 `json:"reclaimed"`
}

type PullResult struct {
	Status string `json:"status"`
}
