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
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	PathEnroll  = "/v1/enroll"
	PathConnect = "/v1/connect"
	PathRenew   = "/v1/renew"
	// PathTransfer + a transfer ID: the sending agent POSTs a tar stream and
	// the receiving agent GETs it; the hub joins the two. See TransferCommand.
	PathTransfer = "/v1/transfer/"

	// ProtocolVersion is sent in Hello; bump on incompatible changes.
	ProtocolVersion = 1
)

// RenewGrace is how long after its certificate expired an agent may still
// renew it (and only renew it: connecting needs an unexpired one), so a host
// that was switched off for a few months comes back without re-enrolling.
const RenewGrace = 180 * 24 * time.Hour

// CertExpiredText is in the hub's reply when it refuses a connection because
// the agent's certificate has expired, so the agent knows to renew it.
const CertExpiredText = "client certificate expired"

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

// RenewRequest asks for a fresh client certificate before the current one
// expires. It is sent over mTLS with the current certificate, which is what
// identifies the host; the CSR only carries the new public key.
type RenewRequest struct {
	CSR []byte `json:"csr"` // DER-encoded PKCS#10
}

// RenewResponse has the same shape as EnrollResponse minus the host ID,
// which the agent already knows.
type RenewResponse struct {
	Cert []byte `json:"cert"` // DER, new client certificate
	CA   []byte `json:"ca"`   // DER, hub CA certificate
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
	// OwnProject is the compose project the agent itself runs in (on the
	// hub's server, the hub's). The agent refuses stack operations on it.
	OwnProject string `json:"own_project,omitempty"`
	// Transfer says the agent can send and receive volume and stack-folder
	// data (TransferCommand). False from older agents.
	Transfer bool `json:"transfer,omitempty"`
	// ContainerMove says the agent can export a container's settings and
	// create a container from them (OpContainerExport, OpContainerCreate).
	ContainerMove bool `json:"container_move,omitempty"`
}

// StackSupport reports the agent's compose setup. Error is set (and stacks
// are unavailable) when something is missing, e.g. the stacks folder mount.
type StackSupport struct {
	Dir            string `json:"dir"`
	ComposeVersion string `json:"compose_version,omitempty"`
	Error          string `json:"error,omitempty"`
	// Files: the agent writes a stack's extra files (Dockerfile, scripts,
	// config) next to its compose file. Older agents would silently drop them.
	Files bool `json:"files,omitempty"`
	// Rollback: the agent records and keeps rollback images and runs
	// StackRollback. Older agents would refuse the command.
	Rollback bool `json:"rollback,omitempty"`
	// Preflight: the agent answers OpStackPreflight. Older agents only
	// have OpStackPreview.
	Preflight bool `json:"preflight,omitempty"`
}

type Container struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	ImageID string            `json:"imageId,omitempty"` // the image it runs (sha256:…)
	State   string            `json:"state"`             // running, exited, paused, restarting, created, dead
	Status  string            `json:"status"`            // human text, e.g. "Up 3 hours (healthy)"
	Created int64             `json:"created"`
	Project string            `json:"project,omitempty"` // compose project label
	Ports   []string          `json:"ports,omitempty"`   // "8080->80/tcp", or "80/tcp" when not published
	Labels  map[string]string `json:"labels,omitempty"`
	// Published lists each host port binding with its host address, one per
	// address as Docker reports them (0.0.0.0 and :: separately). Ports keeps
	// the compact form. Empty from older agents.
	Published []PortBinding `json:"published,omitempty"`
	// HostNetwork: it uses the host's network, so it can listen on any host
	// port without publishing it.
	HostNetwork bool `json:"hostNetwork,omitempty"`
	// NetworkOf: it uses another container's network (network_mode
	// container:<x> or compose's service:<x>), the name or ID Docker gives
	// for that container. Its ports are published on that container, and
	// it loses its network when that container stops or is replaced.
	NetworkOf string `json:"networkOf,omitempty"`
	// Solo: it's the only container of its compose project (Project set).
	// Unless TainerHub manages that project as a stack, it then updates on
	// its own like a docker run container (see UpdatesAlone).
	Solo bool `json:"solo,omitempty"`

	// Live resource use, refreshed about every 10 s for running containers.
	// CPU is a percentage of one core (like `docker stats`), so it can exceed 100.
	CPU      float64 `json:"cpu,omitempty"`
	Mem      int64   `json:"mem,omitempty"`       // bytes, excluding page cache
	MemLimit int64   `json:"mem_limit,omitempty"` // bytes

	// Size is the disk space it uses, as of the agent's last measurement
	// (every half hour: measuring walks the filesystem, so it's never done
	// per snapshot). Nil from older agents and until the first measurement.
	Size *ContainerSize `json:"size,omitempty"`

	// UpdateAvailable: the registry's image for this container's tag (as of
	// the last check, every few hours; see OpImagesCheckUpdates) isn't the
	// image this container runs.
	UpdateAvailable bool `json:"update,omitempty"`
}

// ContainerSize is a container's disk use in bytes.
type ContainerSize struct {
	// RW is what the container wrote itself (its writable layer, Docker's
	// SizeRw), not counting the image it shares with others.
	RW int64 `json:"rw"`
	// Volumes adds up the Docker volumes it mounts (local driver only: a
	// network share's size isn't known). A volume another container also
	// mounts counts for both. Bind mounts aren't measured.
	Volumes int64 `json:"volumes,omitempty"`
	At      int64 `json:"at"` // unix seconds of the measurement
}

// PortBinding is one published port: host address and port to the
// container's port.
type PortBinding struct {
	IP       string `json:"ip,omitempty"` // host address; 0.0.0.0, :: or empty for every address
	HostPort int    `json:"hostPort"`
	Port     int    `json:"port"`  // the container's port
	Proto    string `json:"proto"` // tcp, udp or sctp
}

type Snapshot struct {
	At         int64       `json:"at"` // unix seconds
	Containers []Container `json:"containers"`
	// Host is the whole host's load, measured with the container stats. Nil
	// from older agents and before the first sample.
	Host *HostStats `json:"host,omitempty"`
}

// HostStats is a host's overall load, read from /proc and the stacks
// folder's filesystem.
type HostStats struct {
	At int64 `json:"at"` // unix seconds the sample was taken
	// CPU is the share of all cores busy since the previous sample, 0-100.
	// Nil on the first sample after the agent starts.
	CPU          *float64 `json:"cpu,omitempty"`
	MemAvailable int64    `json:"mem_available,omitempty"` // bytes, MemAvailable from /proc/meminfo
	Load1        float64  `json:"load1,omitempty"`         // 1-minute load average
	DiskFree     int64    `json:"disk_free,omitempty"`     // bytes free on the stacks folder's filesystem
	DiskTotal    int64    `json:"disk_total,omitempty"`
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
	// Transfer sends or receives one volume or stack folder through the hub.
	// Its CommandResult carries a TransferResult; Cancel stops it.
	Transfer *TransferCommand `json:"transfer,omitempty"`
	Cancel   string           `json:"cancel,omitempty"` // ID of a running command to stop
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

// ---------------- data transfers ----------------

// Transfer roles.
const (
	TransferSend    = "send"
	TransferReceive = "receive"
)

// Transfer item kinds.
const (
	// TransferVolume is a named Docker volume using the local driver with no
	// options. The receiver creates it with the same labels, so compose adopts
	// it as its own. Volumes with options (NFS, CIFS, bind) keep their data
	// elsewhere: copying one would write into that same share, so they are
	// refused, and compose recreates them from the compose file instead.
	TransferVolume = "volume"
	// TransferStackDir is a stack's folder under the stacks directory (its
	// relative bind mounts, such as ./data), named by the stack's name.
	TransferStackDir = "stackdir"
)

// TransferRoot is where helper containers mount the data, and so the
// top-level folder of every entry in a transfer's tar stream.
const TransferRoot = "th-data"

// MaxTransferBytes caps one transfer's tar stream (1 TiB).
const MaxTransferBytes = 1 << 40

// TransferCommand asks an agent to send or receive one item. The data is a
// tar stream (every entry under TransferRoot/) that goes from the sender's
// POST to the receiver's GET of PathTransfer+ID on the hub, which ties the
// ID to the two hosts. Both report the stream's size and SHA-256, and the
// hub compares them.
type TransferCommand struct {
	ID       string       `json:"id"`   // transfer ID: 32 lowercase hex characters
	Role     string       `json:"role"` // TransferSend or TransferReceive
	Item     TransferItem `json:"item"`
	MaxBytes int64        `json:"max_bytes"` // abort past this many bytes of tar stream
}

type TransferItem struct {
	Kind string `json:"kind"` // TransferVolume or TransferStackDir
	Name string `json:"name"` // volume name, or stack name for TransferStackDir
	// Labels for the receiver to create the volume with (volumes only).
	Labels map[string]string `json:"labels,omitempty"`
}

// TransferResult is what each side saw of the tar stream.
type TransferResult struct {
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Files  int    `json:"files"` // entries in the archive
}

// ValidTransferID accepts the hub's transfer IDs: 32 lowercase hex characters.
func ValidTransferID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f') {
			return false
		}
	}
	return true
}

func ValidTransferCommand(c TransferCommand) error {
	if !ValidTransferID(c.ID) {
		return errors.New("invalid transfer id")
	}
	if c.Role != TransferSend && c.Role != TransferReceive {
		return errors.New("invalid transfer role")
	}
	if c.MaxBytes <= 0 || c.MaxBytes > MaxTransferBytes {
		return errors.New("invalid transfer size limit")
	}
	switch c.Item.Kind {
	case TransferVolume:
		if !ValidContainerRef(c.Item.Name) {
			return errors.New("invalid volume name")
		}
		if len(c.Item.Labels) > 64 {
			return errors.New("too many volume labels")
		}
		for k, v := range c.Item.Labels {
			if len(k) > 256 || len(v) > 4096 {
				return errors.New("volume label too long")
			}
		}
	case TransferStackDir:
		if !ValidStackName(c.Item.Name) {
			return errors.New("invalid stack name")
		}
		if len(c.Item.Labels) > 0 {
			return errors.New("a stack folder has no labels")
		}
	default:
		return errors.New("unknown transfer item")
	}
	return nil
}

// ---------------- compose stacks ----------------

// Stack actions.
const (
	StackDeploy  = "deploy"  // write the files, then `up -d --remove-orphans`
	StackStart   = "start"   // start existing containers
	StackStop    = "stop"    // stop containers, keep them
	StackRestart = "restart" // restart containers
	StackDown    = "down"    // remove containers and networks (volumes only if Volumes)
	StackRemove  = "remove"  // down, then delete the stack's files on the host
	// StackRollback puts the services in Rollback back on their older
	// images (see rollback.go), without changing the stack's files.
	StackRollback = "rollback"
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
	// Files are the stack's other files (deploy only), written next to the
	// compose file. Files from an earlier deploy that aren't listed any more
	// are removed.
	Files []StackFile `json:"files,omitempty"`
	// Auth are logins for the private registries the compose file's images
	// come from (deploy only). Used for this one command, never stored.
	Auth []RegistryAuth `json:"auth,omitempty"`
	// Rollback lists the services and images a StackRollback goes back to.
	Rollback []RollbackService `json:"rollback,omitempty"`
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
	case StackDeploy, StackStart, StackStop, StackRestart, StackDown, StackRemove, StackRollback:
	default:
		return errors.New("unknown stack action")
	}
	if c.Action == StackRollback {
		if err := ValidRollback(c.Rollback); err != nil {
			return err
		}
	} else if len(c.Rollback) > 0 {
		return errors.New("only a rollback names images to go back to")
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
	if len(c.Auth) > 0 && c.Action != StackDeploy {
		return errors.New("only a deploy pulls images: registry logins aren't allowed here")
	}
	if len(c.Files) > 0 && c.Action != StackDeploy {
		return errors.New("only a deploy writes stack files")
	}
	if err := ValidStackFiles(c.Files); err != nil {
		return err
	}
	return ValidRegistryAuth(c.Auth)
}

// ---------------- registry logins ----------------

// RegistryAuth is a login for one private registry. The hub sends it only
// inside the command that pulls from that registry; agents use it for that
// command and keep nothing.
type RegistryAuth struct {
	Registry string `json:"registry"` // normalized host: ghcr.io, docker.io, myreg.lan:5000
	Username string `json:"username"`
	Password string `json:"password"`
}

// String and LogValue leave the password out, so printing or logging a
// command by mistake can't leak it.
func (a RegistryAuth) String() string { return a.Registry + " (user " + a.Username + ")" }

func (a RegistryAuth) GoString() string { return "wire.RegistryAuth{" + a.String() + "}" }

func (a RegistryAuth) LogValue() slog.Value { return slog.StringValue(a.String()) }

// Limits for registry logins in one command.
const (
	MaxRegistryAuths    = 16
	MaxRegistryHost     = 255
	MaxRegistryUser     = 255
	MaxRegistryPassword = 8 << 10 // tokens (e.g. cloud registries' JWTs) can be long
)

// ValidRegistryHost accepts a normalized registry host with an optional
// port: lowercase letters, digits, '.', '-' and one ':' before the port.
func ValidRegistryHost(s string) bool {
	if s == "" || len(s) > MaxRegistryHost {
		return false
	}
	host, port, hasPort := strings.Cut(s, ":")
	if host == "" || host[0] == '.' || host[0] == '-' || host[len(host)-1] == '.' || host[len(host)-1] == '-' || strings.Contains(host, "..") {
		return false
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
			return false
		}
	}
	if hasPort {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return false
		}
	}
	return true
}

// ValidRegistryAuth checks the logins attached to a command: well-formed
// hosts, each at most once, with a username and a password.
func ValidRegistryAuth(auths []RegistryAuth) error {
	if len(auths) > MaxRegistryAuths {
		return errors.New("too many registry logins")
	}
	seen := map[string]bool{}
	for _, a := range auths {
		if !ValidRegistryHost(a.Registry) || seen[a.Registry] {
			return errors.New("invalid registry login")
		}
		seen[a.Registry] = true
		if a.Username == "" || len(a.Username) > MaxRegistryUser || a.Password == "" || len(a.Password) > MaxRegistryPassword {
			return errors.New("invalid registry login")
		}
	}
	return nil
}

// PullsImages says whether an operation of this kind may pull images (or,
// for OpRegistryLogin, log in; for OpStackPreflight, ask a registry about
// an image), and so may carry registry logins.
func PullsImages(kind string) bool {
	switch kind {
	case OpImagePull, OpContainerRecreate, OpImagesCheckUpdates, OpSelfUpdate, OpRegistryLogin, OpStackPreflight:
		return true
	}
	return false
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

// Next returns the next message's raw JSON without decoding it, so a reader
// can decide whether (and when) a message is worth decoding. The bytes are
// only valid until the next call to Next or Decode.
func (d *Decoder) Next() ([]byte, error) {
	for d.s.Scan() {
		if line := d.s.Bytes(); len(line) > 0 {
			return line, nil
		}
	}
	if err := d.s.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func (d *Decoder) Decode(v any) error {
	line, err := d.Next()
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
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
	// Create is OpContainerCreate's container (and the networks it needs).
	Create *ContainerCreate `json:"create,omitempty"`
	// Share is OpVolumeCreateShare's volume, with the share's login.
	Share *ShareVolume `json:"share,omitempty"`
	// Files are a compose file and .env to check (OpStackPreview only).
	Files *StackFiles `json:"files,omitempty"`
	// Auth are logins for the registries this operation pulls from (only
	// kinds where PullsImages is true). Used for this one operation.
	Auth []RegistryAuth `json:"auth,omitempty"`
}

type StackFiles struct {
	Compose string `json:"compose"`
	Env     string `json:"env,omitempty"`
}

const (
	OpContainerInspect = "container.inspect"
	OpImagesList       = "images.list"
	OpImagePull        = "image.pull"
	OpImageRemove      = "image.remove"
	// ImageRemoveForce is OpImageRemove's Arg for removing an image kept for
	// a stack's rollback (an admin's explicit choice).
	ImageRemoveForce = "force"
	OpImagesPrune    = "images.prune"
	OpVolumesList    = "volumes.list"
	OpVolumeRemove   = "volume.remove"
	OpVolumesPrune   = "volumes.prune"
	// OpVolumeCreateShare creates a volume that mounts a network share
	// (Share), checks that it mounts, and removes it again if it doesn't.
	// Result: ShareCreated.
	OpVolumeCreateShare = "volume.create_share"
	// OpHostInstallShareTools installs the host's package for mounting
	// network shares: cifs-utils for Arg ShareCIFS, nfs-common (nfs-utils)
	// for ShareNFS. Result: ShareTools.
	OpHostInstallShareTools = "host.install_share_tools"
	OpNetworksList          = "networks.list"
	OpNetworkRemove         = "network.remove"
	OpNetworksPrune         = "networks.prune"
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
	// OpHostOSStatus refreshes the host's package lists and lists the OS
	// packages its package manager would upgrade, without installing
	// anything. Result: HostOSStatus.
	OpHostOSStatus = "host.os_status"
	// OpHostOSUpgrade installs every pending OS package update (what
	// OpHostOSStatus lists) through the host's systemd and returns at once.
	// Arg is "install", or OSUpgradeRestartAll when the updates include
	// Docker (which restarts every container). Result: HostOSStatus of what
	// was started.
	OpHostOSUpgrade = "host.os_upgrade"
	// OSUpgradeRestartAll is OpHostOSUpgrade's Arg accepting Docker restarts.
	OSUpgradeRestartAll = "restart-all"
	// OpStackInspect describes stack Target on this host for moving it: its
	// volumes (with sizes), bind mounts, published ports and images, read from
	// `compose config`. Works for a stack not deployed here too (Deployed is
	// then false), to check the folder is free. Result: StackInspect.
	OpStackInspect = "stack.inspect"
	// OpStackDirRemove deletes the data in stack Target's folder (whatever
	// its owners) and the folder itself. Refused while the stack is deployed
	// on this host (its compose file is there). Used to clean up after a move.
	OpStackDirRemove = "stack.remove_dir"
	// OpStackPreview compares Files (a compose file and .env for stack
	// Target) with the compose project of that name running on the host,
	// without changing anything: what the file resolves to in the stack's
	// folder, and what the running containers use now. Used before taking
	// over a project started outside TainerHub. Result: StackPreview.
	OpStackPreview = "stack.preview"
	// OpStackRead reads the compose project Target as it was started outside
	// TainerHub, from the folder and files its containers' labels name, and
	// merges it with `docker compose config --no-interpolate` into one file
	// whose relative paths are absolute (so its data stays where it is).
	// Files given with env_file are copied into the stack's folder and the
	// file points at the copies. Result: StackSource.
	OpStackRead = "stack.read"
	// OpContainerExport describes standalone container Target for moving it
	// to another host: the body to create it again (what Pull & recreate
	// uses), its volumes with sizes, host folders, networks and published
	// ports. Result: ContainerExport.
	OpContainerExport = "container.export"
	// OpContainerCreate creates (and with Start, starts) the container in
	// Create, first creating any of its bridge networks this host lacks.
	// Refused for compose containers and TainerHub's own names. Result:
	// ContainerCreateResult.
	OpContainerCreate = "container.create"
	// OpRegistryLogin checks the one login in Auth against its registry
	// through the Docker Engine (POST /auth), without saving it on the host.
	// Result: RegistryLoginResult.
	OpRegistryLogin = "registry.login"
	// OpStackImages lists the image each service of compose project Target
	// runs (from its containers). With Arg StackImagesProtect it also tags
	// those images tainerhub-rollback/<stack>:<service> so pruning leaves
	// them, and drops that stack's older rollback tags. Result: StackImages.
	OpStackImages = "stack.images"
	// OpStackPreflight checks Files (a compose file and .env for new stack
	// Target) against this host before a first deploy, without changing
	// anything: what `compose config` says, each service's published ports,
	// limits, devices and GPUs, whether its image exists in its registry
	// (with Auth) for this host's platform, and which external networks and
	// volumes exist. Result: StackPreflight.
	OpStackPreflight = "stack.preflight"
	// OpBindsList lists the host folders containers mount (bind mounts),
	// leaving out Docker's socket and system paths, so a new container can be
	// pointed at folders that follow the host's own layout. Result: []BindMount.
	OpBindsList = "binds.list"
)

// BindMount is one host folder a container mounts.
type BindMount struct {
	Container string `json:"container"`
	Image     string `json:"image"`
	Source    string `json:"source"` // on the host
	Target    string `json:"target"` // in the container
}

// StackPreflight is what OpStackPreflight found.
type StackPreflight struct {
	// ConfigError is set when `compose config` rejects the files; the rest
	// is then empty.
	ConfigError string             `json:"config_error,omitempty"`
	Warnings    []string           `json:"warnings,omitempty"`
	Dir         string             `json:"dir"`
	Services    []PreflightService `json:"services,omitempty"`
	// External networks and volumes the file expects to exist already.
	Networks []PreflightResource `json:"networks,omitempty"`
	Volumes  []PreflightResource `json:"volumes,omitempty"`
	// GPURuntime: Docker can hand NVIDIA GPUs to containers (the nvidia
	// runtime or CDI devices are set up).
	GPURuntime bool   `json:"gpu_runtime"`
	Arch       string `json:"arch"` // the host's, as Docker names platforms (amd64, arm64, arm/v7)
}

type PreflightService struct {
	Name          string `json:"name"`
	ContainerName string `json:"container_name,omitempty"`
	Image         string `json:"image,omitempty"`
	Build         bool   `json:"build,omitempty"`
	HostNetwork   bool   `json:"host_network,omitempty"`
	// NetworkOf is the service or container whose network it uses
	// (network_mode: service:<x> or container:<x>).
	NetworkOf  string          `json:"network_of,omitempty"`
	Ports      []PreflightPort `json:"ports,omitempty"`
	CPUs       float64         `json:"cpus,omitempty"`
	Memory     int64           `json:"memory,omitempty"` // bytes
	GPU        bool            `json:"gpu,omitempty"`
	Devices    []string        `json:"devices,omitempty"` // host paths
	Binds      []StackBind     `json:"binds,omitempty"`
	ImageCheck *ImageCheck     `json:"image_check,omitempty"`
	// PlaceholderEnv names the environment variables whose values look like
	// examples from the docs ("changeme", "<your-domain>"; see
	// LooksLikePlaceholder). Their values aren't sent.
	PlaceholderEnv []string `json:"placeholder_env,omitempty"`
}

type PreflightPort struct {
	HostIP    string `json:"host_ip,omitempty"`
	Published string `json:"published"` // a port or a range like 8000-8010
	Target    string `json:"target"`
	Protocol  string `json:"protocol"`
}

type PreflightResource struct {
	Name   string `json:"name"`
	Exists bool   `json:"exists"`
}

// ImageCheck is what the host and the image's registry say about an image.
type ImageCheck struct {
	Local bool `json:"local"` // already on the host
	// Remote: "found", "not_found", "denied" (needs a login) or "error".
	Remote    string   `json:"remote"`
	Error     string   `json:"error,omitempty"`
	Platforms []string `json:"platforms,omitempty"` // e.g. linux/amd64, linux/arm/v7
}

// RegistryLoginResult is what the registry said to a login check.
type RegistryLoginResult struct {
	Status string `json:"status"`
}

// StackSource is a running project's compose setup, read from its host.
type StackSource struct {
	Compose     string   `json:"compose"`            // the merged file, variables left as ${...}
	Env         string   `json:"env"`                // its .env (or --env-file), as written
	WorkingDir  string   `json:"working_dir"`        // the folder it was started from
	ConfigFiles []string `json:"config_files"`       // the compose files that were merged
	EnvFile     string   `json:"env_file,omitempty"` // where Env came from, if anywhere
	Copied      []string `json:"copied,omitempty"`   // env_file files copied into the stack's folder
	Warnings    []string `json:"warnings,omitempty"` // compose's
}

// ContainerExport describes a standalone container on its host.
type ContainerExport struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`    // reference it was created from
	ImageID string `json:"image_id"` // image it runs
	Running bool   `json:"running"`
	Project string `json:"project,omitempty"` // compose project label, if any
	// Spec is the body for Docker's POST /containers/create: the
	// container's own settings, with every volume (anonymous ones too)
	// mounted by name.
	Spec        json.RawMessage `json:"spec"`
	Volumes     []ExportVolume  `json:"volumes"`
	Binds       []string        `json:"binds"` // host folders it mounts
	Networks    []NetworkSpec   `json:"networks"`
	NetworkMode string          `json:"network_mode,omitempty"`
	Ports       []string        `json:"ports"` // published host ports, e.g. "8080/tcp"
	Devices     bool            `json:"devices,omitempty"`
	Privileged  bool            `json:"privileged,omitempty"`
	// Problems are settings that tie it to this host (another container's
	// network or volumes, say), so it can't move.
	Problems []string `json:"problems,omitempty"`
}

type ExportVolume struct {
	Name        string            `json:"name"`
	Destination string            `json:"destination"`
	Anonymous   bool              `json:"anonymous,omitempty"`
	Driver      string            `json:"driver,omitempty"`
	Options     bool              `json:"options,omitempty"` // mounts storage from elsewhere (NFS, CIFS)
	Size        int64             `json:"size"`              // bytes; -1 if unknown
	Labels      map[string]string `json:"labels,omitempty"`
	UsedBy      []string          `json:"used_by,omitempty"` // other containers mounting it
}

// NetworkSpec is a network a container is attached to, as needed to create
// it on another host.
type NetworkSpec struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Builtin    bool              `json:"builtin,omitempty"` // bridge, host, none
	Internal   bool              `json:"internal,omitempty"`
	Attachable bool              `json:"attachable,omitempty"`
	IPv6       bool              `json:"ipv6,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	IPAM       json.RawMessage   `json:"ipam,omitempty"` // Docker's IPAM block, as is
}

// ContainerCreate is OpContainerCreate's request.
type ContainerCreate struct {
	Name     string          `json:"name"`
	Spec     json.RawMessage `json:"spec"`
	Networks []NetworkSpec   `json:"networks,omitempty"` // created first if missing
	Start    bool            `json:"start,omitempty"`
}

type ContainerCreateResult struct {
	ID       string   `json:"id"`
	Networks []string `json:"networks,omitempty"` // networks it created
}

// MaxContainerSpec caps a container's create body.
const MaxContainerSpec = 1 << 20

// ValidContainerCreate checks what can be checked without Docker.
func ValidContainerCreate(c *ContainerCreate) error {
	if c == nil {
		return errors.New("nothing to create")
	}
	if !ValidContainerRef(c.Name) || strings.HasPrefix(c.Name, "tainerhub") {
		return errors.New("invalid container name")
	}
	if len(c.Spec) == 0 || len(c.Spec) > MaxContainerSpec {
		return errors.New("invalid container settings")
	}
	var spec struct {
		Image  string
		Labels map[string]string
	}
	if err := json.Unmarshal(c.Spec, &spec); err != nil {
		return errors.New("unreadable container settings")
	}
	if !ValidImageRef(spec.Image) {
		return errors.New("invalid image reference")
	}
	if spec.Labels["com.docker.compose.project"] != "" {
		return errors.New("compose containers are created by their stack")
	}
	if len(c.Networks) > 16 {
		return errors.New("too many networks")
	}
	for _, n := range c.Networks {
		if !ValidContainerRef(n.Name) || n.Builtin {
			return errors.New("invalid network name")
		}
		if n.Driver != "bridge" {
			return fmt.Errorf("network %s uses the %s driver; only bridge networks are created", n.Name, n.Driver)
		}
	}
	return nil
}

// StackPreview is what OpStackPreview found.
type StackPreview struct {
	// ConfigError is set when `compose config` rejects the file in the
	// stack's folder (e.g. a relative env_file that isn't there).
	ConfigError string `json:"config_error,omitempty"`
	// Warnings are compose's own, such as variables that aren't set.
	Warnings []string       `json:"warnings,omitempty"`
	Services []StackService `json:"services,omitempty"`
	Mounts   []StackMount   `json:"mounts,omitempty"`
	// Dir is the stack's folder; DirFiles counts what's already in it.
	Dir      string `json:"dir"`
	DirFiles int    `json:"dir_files"`
	// Paths says, for each bind source inside Dir, how many entries it
	// holds (-1: it doesn't exist; 0: an empty folder; 1 for a file).
	Paths map[string]int `json:"paths,omitempty"`

	// The project as it runs now.
	Running     []RunningContainer `json:"running,omitempty"`
	WorkingDir  string             `json:"working_dir,omitempty"`  // folder it was started from
	ConfigFiles []string           `json:"config_files,omitempty"` // compose files it was started with
}

// StackMount is one mount the compose file gives a service: a host path
// for binds, a Docker volume name for volumes.
type StackMount struct {
	Service string `json:"service"`
	Type    string `json:"type"` // bind or volume
	Source  string `json:"source"`
	Target  string `json:"target"`
}

type RunningContainer struct {
	Name    string        `json:"name"`
	Service string        `json:"service"`
	Mounts  []DetailMount `json:"mounts"`
}

// StackInspect describes a stack's data and needs on one host.
type StackInspect struct {
	Deployed bool `json:"deployed"` // the stack's compose file is in its folder
	// Dir is the stack's folder; DirFiles counts what's in it besides the
	// compose file, .env and TainerHub's own temporary folders.
	Dir      string `json:"dir"`
	DirFiles int    `json:"dir_files"`
	// DirSize is the folder's size in bytes; DirSizePartial means some of
	// it couldn't be read (files of other users), so it's a lower bound.
	DirSize        int64 `json:"dir_size"`
	DirSizePartial bool  `json:"dir_size_partial,omitempty"`

	Services []StackService `json:"services,omitempty"`
	Volumes  []StackVolume  `json:"volumes,omitempty"`
	Binds    []StackBind    `json:"binds,omitempty"`
	// Ports are the host ports the stack publishes, e.g. "8080/tcp".
	Ports []string `json:"ports,omitempty"`
}

type StackService struct {
	Name        string `json:"name"`
	Image       string `json:"image,omitempty"`
	Build       bool   `json:"build,omitempty"`        // has a build: section
	HostNetwork bool   `json:"host_network,omitempty"` // network_mode: host
	Devices     bool   `json:"devices,omitempty"`      // maps host devices (USB sticks, GPUs)
}

type StackVolume struct {
	Key  string `json:"key"`  // name in the compose file
	Name string `json:"name"` // Docker volume name
	// External volumes are made outside the stack; Options means it mounts
	// storage from elsewhere (NFS, CIFS, a bind): neither is copied.
	External bool   `json:"external,omitempty"`
	Driver   string `json:"driver,omitempty"`
	Options  bool   `json:"options,omitempty"`
	Exists   bool   `json:"exists"`
	Size     int64  `json:"size"` // bytes; -1 if Docker didn't say
	// Labels as Docker has them, to recreate the volume elsewhere.
	Labels map[string]string `json:"labels,omitempty"`
}

type StackBind struct {
	Service string `json:"service"`
	Source  string `json:"source"` // host path
	Target  string `json:"target"`
	// InStackDir: inside the stack's folder, so it moves with it.
	InStackDir bool `json:"in_stack_dir"`
}

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
	// RefreshFailed: the package lists couldn't be refreshed, so Candidate
	// may be out of date. RefreshError says why (apt's own words).
	RefreshFailed bool   `json:"refresh_failed,omitempty"`
	RefreshError  string `json:"refresh_error,omitempty"`
	// RepoMissing: no repository offers Docker (apt lists only the
	// installed copy), often because an OS release upgrade turned Docker's
	// source off. Fix, when set, is the command that turns it back on.
	RepoMissing bool   `json:"repo_missing,omitempty"`
	Fix         string `json:"fix,omitempty"`
	// Warning: something worth knowing that doesn't stop the upgrade.
	Warning string `json:"warning,omitempty"`
}

// HostOSStatus is what the host's package manager would upgrade.
type HostOSStatus struct {
	OS             string      `json:"os,omitempty"` // e.g. Debian GNU/Linux 12 (bookworm)
	Kernel         string      `json:"kernel,omitempty"`
	Manager        string      `json:"manager"` // apt, dnf, yum, unknown
	Packages       []OSPackage `json:"packages"`
	Security       int         `json:"security"` // how many of Packages are security updates
	RebootRequired bool        `json:"reboot_required"`
	RebootPackages []string    `json:"reboot_packages,omitempty"` // what asked for the reboot (apt)
	RefreshFailed  bool        `json:"refresh_failed,omitempty"`
	RefreshError   string      `json:"refresh_error,omitempty"`
	Systemd        bool        `json:"systemd"`
	// CanInstall: TainerHub can install the updates (OpHostOSUpgrade).
	CanInstall bool `json:"can_install"`
	// RestartsDocker: the updates include Docker or containerd, so
	// installing them restarts every container.
	RestartsDocker bool `json:"restarts_docker,omitempty"`
	// UpgradeRunning: TainerHub is installing updates right now (the list
	// isn't refreshed meanwhile).
	UpgradeRunning bool             `json:"upgrade_running,omitempty"`
	LastUpgrade    *OSUpgradeResult `json:"last_upgrade,omitempty"`
}

// OSUpgradeResult is how the last install TainerHub started ended.
type OSUpgradeResult struct {
	At    int64  `json:"at"`   // unix seconds
	Exit  int    `json:"exit"` // the package manager's exit status
	Error string `json:"error,omitempty"`
}

type OSPackage struct {
	Name     string `json:"name"`
	From     string `json:"from,omitempty"` // installed version; empty for a new package
	To       string `json:"to"`
	Origin   string `json:"origin,omitempty"` // e.g. Debian-Security:12/stable-security
	Security bool   `json:"security,omitempty"`
}

// ImageUpdates maps image references (as containers use them) to whether a
// newer image is available.
type ImageUpdates struct {
	Checked int64             `json:"checked"` // unix seconds
	Images  map[string]bool   `json:"images"`
	Errors  map[string]string `json:"errors,omitempty"`
	// Containers says, per container ID, whether the image that container
	// runs differs from the registry's: a container can be behind even when
	// the tag on disk is current (another stack, or a plain pull, fetched it).
	// Missing from older agents; then Images is all there is.
	Containers map[string]bool `json:"containers,omitempty"`
	// Remote is the registry's digest for each image checked. The agent
	// keeps it to compare each container's image with.
	Remote map[string]string `json:"-"`
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
	OpDockerPlan: false, OpDockerUpgrade: true, OpHostOSStatus: false, OpHostOSUpgrade: true,
	OpStackInspect: false, OpStackDirRemove: true, OpStackPreview: false, OpStackRead: true,
	OpContainerExport: false, OpContainerCreate: true,
	OpRegistryLogin:         false,
	OpStackImages:           true, // with StackImagesProtect it tags images
	OpStackPreflight:        false,
	OpBindsList:             false,
	OpVolumeCreateShare:     true,
	OpHostInstallShareTools: true,
}

// ValidOperation checks the kind and the shape of its target / argument.
func ValidOperation(op Operation) error {
	if _, ok := OpKinds[op.Kind]; !ok {
		return errors.New("unknown operation")
	}
	if len(op.Auth) > 0 && !PullsImages(op.Kind) {
		return errors.New("this operation doesn't pull images: registry logins aren't allowed")
	}
	if err := ValidRegistryAuth(op.Auth); err != nil {
		return err
	}
	switch op.Kind {
	case OpRegistryLogin:
		if len(op.Auth) != 1 {
			return errors.New("a login check needs exactly one registry login")
		}
	case OpContainerInspect, OpVolumeRemove, OpNetworkRemove, OpContainerRecreate, OpContainerExport:
		if !ValidContainerRef(op.Target) {
			return errors.New("invalid target")
		}
	case OpImageRemove:
		if !ValidImageID(op.Target) {
			return errors.New("invalid image id")
		}
		if op.Arg != "" && op.Arg != ImageRemoveForce {
			return errors.New("invalid remove mode")
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
	case OpHostOSUpgrade:
		if op.Arg != "install" && op.Arg != OSUpgradeRestartAll {
			return errors.New("invalid install mode")
		}
	case OpDockerUpgrade:
		if op.Arg != "restart-all" {
			return errors.New("upgrading Docker restarts every container: it needs an explicit OK")
		}
	case OpStackInspect, OpStackDirRemove, OpStackRead:
		if !ValidStackName(op.Target) {
			return errors.New("invalid stack name")
		}
	case OpStackImages:
		if !ValidStackName(op.Target) {
			return errors.New("invalid stack name")
		}
		if op.Arg != "" && op.Arg != StackImagesProtect {
			return errors.New("invalid stack images mode")
		}
	case OpStackPreview, OpStackPreflight:
		if !ValidStackName(op.Target) {
			return errors.New("invalid stack name")
		}
		if op.Files == nil {
			return errors.New("no compose file to check")
		}
		if err := ValidStackCommand(StackCommand{Action: StackDeploy, Name: op.Target, Compose: op.Files.Compose, Env: op.Files.Env}); err != nil {
			return err
		}
	case OpImagesPrune, OpVolumesPrune, OpNetworksPrune:
		if op.Arg != "" && op.Arg != "all" {
			return errors.New("invalid prune mode")
		}
	case OpContainerCreate:
		if op.Share != nil {
			return errors.New("only volume.create_share takes a share")
		}
		return ValidContainerCreate(op.Create)
	case OpHostInstallShareTools:
		if op.Arg != ShareCIFS && op.Arg != ShareNFS {
			return errors.New("install tools for cifs or nfs")
		}
	case OpVolumeCreateShare:
		if op.Create != nil {
			return errors.New("only container.create takes a container")
		}
		return ValidShareVolume(op.Share)
	}
	if op.Create != nil {
		return errors.New("only container.create takes a container")
	}
	if op.Share != nil {
		return errors.New("only volume.create_share takes a share")
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
	Container string `json:"container"`      // e.g. "80/tcp"
	Host      string `json:"host,omitempty"` // host port, prefixed with the address when it's one address: "8080", "127.0.0.1:8080"
	IP        string `json:"ip,omitempty"`   // the host address as Docker reports it (0.0.0.0 or :: for every address)
}

type Image struct {
	ID      string   `json:"id"`
	Tags    []string `json:"tags"`
	Size    int64    `json:"size"`
	Created int64    `json:"created"`
	UsedBy  []string `json:"usedBy"`
	// KeptFor names the stacks this image is kept for, to roll back to
	// (it has a tainerhub-rollback/ tag). TainerHub's prunes leave it.
	KeptFor []string `json:"keptFor,omitempty"`
}

type Volume struct {
	Name       string   `json:"name"`
	Driver     string   `json:"driver"`
	Mountpoint string   `json:"mountpoint"`
	Created    string   `json:"created"`
	Project    string   `json:"project,omitempty"`
	UsedBy     []string `json:"usedBy"`
	// Share is set when the volume mounts a network share (never with
	// its options, which can hold the share's password).
	Share *ShareInfo `json:"share,omitempty"`
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
	// Kept lists unused items Docker refused to remove, with its reason
	// (pruning every unused image, and network share volumes a full volume
	// prune leaves alone).
	Kept []KeptItem `json:"kept,omitempty"`
}

type KeptItem struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type PullResult struct {
	Status string `json:"status"`
}

// SharedNetworkTarget is the container (or compose service) a network mode
// joins: "gluetun" for "container:gluetun" or "service:gluetun", "" for
// any other mode.
func SharedNetworkTarget(mode string) string {
	for _, p := range []string{"container:", "service:"} {
		if t, ok := strings.CutPrefix(mode, p); ok {
			return t
		}
	}
	return ""
}

// UsesNetworkOf says whether dep runs in gw's network (dep.NetworkOf names
// gw by name or by ID, which Docker may give in full or shortened).
func UsesNetworkOf(dep, gw Container) bool {
	t := dep.NetworkOf
	if t == "" || dep.ID == gw.ID {
		return false
	}
	return t == gw.Name || (len(t) >= 12 && strings.HasPrefix(gw.ID, t))
}

// NetworkUsers are the containers in cs that run in gw's network, by name.
// UpdatesAlone says whether c can be updated by itself (pull and recreate)
// rather than with its stack: it's not in a compose project, or it's the
// only container of one. A project TainerHub manages as a stack still
// updates with Pull & redeploy; callers check that (managed).
func UpdatesAlone(c Container, managed bool) bool {
	return c.Project == "" || (c.Solo && !managed)
}

func NetworkUsers(gw Container, cs []Container) []string {
	var out []string
	for _, c := range cs {
		if UsesNetworkOf(c, gw) {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}
