// Package group coordinates compose projects: sets of Waypoint sessions, one
// per service, whose forks talk to each other over a per-replica fabric.
//
// The model is built from the session model rather than beside it:
//
//	project            = one session per service + a record of their addresses
//	replica R          = the fork named R in every member session, all attached
//	                     to fabric R with the project's address plan
//	group checkpoint C = checkpoint C in every member session + a manifest
//
// Every replica's fabric uses the same addresses, so connections between
// services that a checkpoint holds restore into any replica forked from it.
// A group checkpoint is taken behind a fabric barrier, which makes the member
// snapshots one consistent cut of those connections.
//
// State lives under <session registry>/groups/<project>/: group.json, one
// record per live replica and per group checkpoint, and the locks. Fabric
// namespaces are pinned under <net runtime>/groups/<project>/<replica>/.
package group

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Alex-XJK/waypoint/pkg/waypoint"
)

// MainReplica is the replica `up` starts: every member session's main fork.
const MainReplica = waypoint.MainForkID

// DefaultSubnet is the fabric subnet when a spec names none.
const DefaultSubnet = "10.89.0.0/24"

// Spec is a project to bring up. Services start in slice order, which callers
// make a dependency order.
type Spec struct {
	Name     string
	Subnet   string
	Services []Service
}

// Service is one member of a project: its rootfs comes from Rootfs (a
// directory, as for `init`) or Build (as for `build`).
type Service struct {
	Name   string
	Rootfs string
	Build  *waypoint.BuildSpec
	// Entrypoint and Command follow compose semantics: nil means unset, so
	// the image's ENTRYPOINT/CMD apply; setting Entrypoint also drops the
	// image's CMD. An empty resulting command starts nothing.
	Entrypoint []string
	Command    []string
	Env        []string
	WorkDir    string
}

// Record is a project's persistent description.
type Record struct {
	Name      string          `json:"name"`
	Subnet    string          `json:"subnet"`
	Services  []ServiceRecord `json:"services"`
	Status    ProjectStatus   `json:"status"`
	CreatedAt int64           `json:"created_at"`
}

// ServiceRecord ties a service to its session and fabric address.
type ServiceRecord struct {
	Name      string `json:"name"`
	SessionID string `json:"session_id,omitempty"`
	Address   string `json:"address"`
	// Command is the bash command line `up` started the service with.
	Command string `json:"command,omitempty"`
}

// Replica is a live replica of the project.
type Replica struct {
	ID         string        `json:"id"`
	Checkpoint string        `json:"checkpoint,omitempty"`
	Fabric     string        `json:"fabric"`
	Status     ReplicaStatus `json:"status"`
	CreatedAt  int64         `json:"created_at"`
}

// Checkpoint is a group checkpoint's manifest: checkpoint ID in every member.
type Checkpoint struct {
	ID          string           `json:"id"`
	Parent      string           `json:"parent,omitempty"`
	FromReplica string           `json:"from_replica"`
	Status      CheckpointStatus `json:"status"`
	Error       string           `json:"error,omitempty"`
	CreatedAt   int64            `json:"created_at"`
}

// ProjectStatus is a project's lifecycle: creating while `up` runs, then
// ready. Only cleanup accepts a project that is not ready.
type ProjectStatus string

const (
	ProjectCreating ProjectStatus = "creating"
	ProjectReady    ProjectStatus = "ready"
)

// ReplicaStatus is a replica's lifecycle: creating while its members are
// forked, then running. Only a running replica can be snapshotted.
type ReplicaStatus string

const (
	ReplicaCreating ReplicaStatus = "creating"
	ReplicaRunning  ReplicaStatus = "running"
	// ReplicaDegraded marks a replica whose members may no longer agree,
	// after a failed group snapshot.
	ReplicaDegraded ReplicaStatus = "degraded"
)

// CheckpointStatus is a group checkpoint's lifecycle: creating while its
// members are snapshotted, then ready (forkable) or failed.
type CheckpointStatus string

const (
	CheckpointCreating CheckpointStatus = "creating"
	CheckpointReady    CheckpointStatus = "ready"
	CheckpointFailed   CheckpointStatus = "failed"
)

// Listing is the stable JSON shape of `compose list <project> --json`.
type Listing struct {
	Record      Record       `json:"project"`
	Replicas    []Replica    `json:"replicas"`
	Checkpoints []Checkpoint `json:"checkpoints"`
}

// --- paths ---

func groupsDir() string {
	_, infoDir := waypoint.StateDirs()
	return filepath.Join(infoDir, "groups")
}

func projectDir(name string) string      { return filepath.Join(groupsDir(), name) }
func recordPath(name string) string      { return filepath.Join(projectDir(name), "group.json") }
func replicasDir(name string) string     { return filepath.Join(projectDir(name), "replicas") }
func checkpointsDir(name string) string  { return filepath.Join(projectDir(name), "checkpoints") }
func groupLockPath(name string) string   { return filepath.Join(projectDir(name), "locks", "group.lock") }
func netRuntimeDir(name string) string   { return filepath.Join(waypoint.NetRuntimeDir, "groups", name) }
func fabricPin(name, rep string) string  { return filepath.Join(netRuntimeDir(name), rep, "netns") }
func replicaPath(name, id string) string { return filepath.Join(replicasDir(name), id+".json") }
func checkpointPath(name, id string) string {
	return filepath.Join(checkpointsDir(name), id+".json")
}
func replicaLockPath(name, id string) string {
	return filepath.Join(projectDir(name), "locks", "replica-"+id+".lock")
}

// --- persistence ---

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return waypoint.AtomicWriteFile(path, data, 0o644)
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// ErrNotFound reports a project, replica, or checkpoint that does not exist.
var ErrNotFound = errors.New("not found")

// LoadRecord returns project name's record.
func LoadRecord(name string) (*Record, error) {
	if err := waypoint.ValidateID("project", name); err != nil {
		return nil, err
	}
	var r Record
	if err := readJSON(recordPath(name), &r); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("project %s: %w", name, ErrNotFound)
		}
		return nil, err
	}
	return &r, nil
}

// MemberService returns the service that session sessionID provides in
// project, or "" when no such project lists it. A session keeps the project
// name it was created for, so this tells a live member from a leftover of a
// removed (or since recreated) project.
func MemberService(project, sessionID string) (string, error) {
	rec, err := LoadRecord(project)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, s := range rec.Services {
		if s.SessionID == sessionID {
			return s.Name, nil
		}
	}
	return "", nil
}

// IsProjectFabric reports whether the namespace path pin lies under the
// directory where projects pin their replica fabrics. Those fabrics carry the
// project's address plan, which only the project's own replicas may use.
func IsProjectFabric(pin string) bool {
	p, err := filepath.Abs(pin)
	if err != nil {
		return false
	}
	base := filepath.Join(waypoint.NetRuntimeDir, "groups")
	for _, path := range []*string{&p, &base} {
		if resolved, err := filepath.EvalSymlinks(*path); err == nil {
			*path = resolved
		}
	}
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, "../")
}

func loadReplica(name, id string) (*Replica, error) {
	if err := waypoint.ValidateID("replica", id); err != nil {
		return nil, err
	}
	var r Replica
	if err := readJSON(replicaPath(name, id), &r); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("replica %s of project %s: %w", id, name, ErrNotFound)
		}
		return nil, err
	}
	return &r, nil
}

func loadCheckpoint(name, id string) (*Checkpoint, error) {
	if err := waypoint.ValidateID("checkpoint", id); err != nil {
		return nil, err
	}
	var c Checkpoint
	if err := readJSON(checkpointPath(name, id), &c); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("checkpoint %s of project %s: %w", id, name, ErrNotFound)
		}
		return nil, err
	}
	return &c, nil
}

// readAll decodes every *.json in dir into a T, sorted by file name.
func readAll[T any](dir string) ([]T, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []T{}, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := make([]T, 0, len(names))
	for _, n := range names {
		var v T
		if err := readJSON(filepath.Join(dir, n), &v); err != nil {
			continue // removed concurrently, or a torn record
		}
		out = append(out, v)
	}
	return out, nil
}

// ListProjects returns the names of all projects.
func ListProjects() ([]string, error) {
	entries, err := os.ReadDir(groupsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	names := []string{}
	for _, e := range entries {
		if e.IsDir() && waypoint.ValidateID("project", e.Name()) == nil {
			if _, err := os.Stat(recordPath(e.Name())); err == nil {
				names = append(names, e.Name())
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

// List returns project name's record, live replicas, and group checkpoints.
func List(name string) (*Listing, error) {
	rec, err := LoadRecord(name)
	if err != nil {
		return nil, err
	}
	replicas, err := readAll[Replica](replicasDir(name))
	if err != nil {
		return nil, err
	}
	ckpts, err := readAll[Checkpoint](checkpointsDir(name))
	if err != nil {
		return nil, err
	}
	return &Listing{Record: *rec, Replicas: replicas, Checkpoints: ckpts}, nil
}

// --- spec validation and the address plan ---

// plan validates spec and returns the service records with their addresses:
// the subnet's .2, .3, ... in service order (.1 is left for a gateway).
func plan(spec Spec) ([]ServiceRecord, string, error) {
	if err := waypoint.ValidateID("project", spec.Name); err != nil {
		return nil, "", err
	}
	if len(spec.Services) == 0 {
		return nil, "", fmt.Errorf("project %s has no services", spec.Name)
	}
	subnet := spec.Subnet
	if subnet == "" {
		subnet = DefaultSubnet
	}
	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil || ipNet.IP.To4() == nil {
		return nil, "", fmt.Errorf("invalid subnet %q: want an IPv4 CIDR", subnet)
	}
	ones, _ := ipNet.Mask.Size()
	if hosts := max(1<<(32-ones)-3, 0); len(spec.Services) > hosts {
		return nil, "", fmt.Errorf("subnet %s has room for %d services, not %d", subnet, hosts, len(spec.Services))
	}
	base := ipNet.IP.To4()
	seen := map[string]bool{}
	records := make([]ServiceRecord, 0, len(spec.Services))
	for i, svc := range spec.Services {
		if err := validServiceName(svc.Name); err != nil {
			return nil, "", err
		}
		if seen[svc.Name] {
			return nil, "", fmt.Errorf("duplicate service %q", svc.Name)
		}
		seen[svc.Name] = true
		if (svc.Rootfs == "") == (svc.Build == nil) {
			return nil, "", fmt.Errorf("service %s needs exactly one of a rootfs directory or a build", svc.Name)
		}
		if err := guestOptions(svc, nil).Validate(); err != nil {
			return nil, "", fmt.Errorf("service %s: %w", svc.Name, err)
		}
		ip := make(net.IP, 4)
		copy(ip, base)
		n := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
		n += uint32(i + 2)
		ip[0], ip[1], ip[2], ip[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
		records = append(records, ServiceRecord{Name: svc.Name, Address: fmt.Sprintf("%s/%d", ip, ones)})
	}
	return records, ipNet.String(), nil
}

// validServiceName accepts compose service names that also work as the
// service's hostname and /etc/hosts name (at most 63 bytes).
func validServiceName(name string) error {
	if err := waypoint.ValidateID("service", name); err != nil {
		return err
	}
	if len(name) > 63 {
		return fmt.Errorf("invalid service name %q: longer than 63 bytes", name)
	}
	return nil
}

// guestOptions are the guest settings of svc's session: the service's
// environment and working directory, its name as hostname, and hosts.
func guestOptions(svc Service, hosts []string) waypoint.GuestOptions {
	return waypoint.GuestOptions{Env: svc.Env, WorkDir: svc.WorkDir, Hostname: svc.Name, Hosts: hosts}
}

// hostsLines maps every service name to its address, for /etc/hosts.
func hostsLines(services []ServiceRecord) []string {
	lines := make([]string, 0, len(services))
	for _, s := range services {
		ip, _, _ := strings.Cut(s.Address, "/")
		lines = append(lines, ip+" "+s.Name)
	}
	return lines
}

// serviceArgv resolves a service's command against its image's defaults.
func serviceArgv(svc Service, imageEntrypoint, imageCmd []string) []string {
	entrypoint, cmd := imageEntrypoint, imageCmd
	if svc.Entrypoint != nil {
		entrypoint, cmd = svc.Entrypoint, nil
	}
	if svc.Command != nil {
		cmd = svc.Command
	}
	return append(append([]string(nil), entrypoint...), cmd...)
}

// serviceLog is where a service's output goes, inside its rootfs.
const serviceLog = "/.waypoint/service.log"

// startCommand is the bash command line that runs argv as a background job of
// the member's persistent shell, so the process tree (and the service) is part
// of every checkpoint. "" when there is nothing to run.
func startCommand(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ") + " >" + serviceLog + " 2>&1 </dev/null &"
}

// newReplicaID returns a fresh generated replica ID.
func newReplicaID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "r-" + hex.EncodeToString(b), nil
}
