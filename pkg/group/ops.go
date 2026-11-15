package group

// Project operations. Each fans out over the member sessions in parallel
// through the session API, so per-session locking, configuration, and
// failure handling are the ones every other command uses.
//
// Locking, outermost first:
//
//   - the project lock is held for a whole operation: shared by those that
//     work on replicas (fork, snapshot, destroy), exclusive by those that
//     create or end the project's forks as a whole (up, suspend, cleanup), so
//     the latter never run under an operation in flight;
//   - a replica lock is held around the long work on one replica (restores,
//     snapshots, teardown);
//   - the group lock guards short metadata transitions (creating and removing
//     replica and checkpoint records) and is never held while a replica lock
//     is taken;
//   - member sessions take their own session and fork locks inside.
//
// Exec takes none of the project's locks; the member's fork lock serializes
// it with whatever else happens to that fork.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/Alex-XJK/waypoint/pkg/waypoint"
)

// Logger receives progress messages; a nil Logger discards them.
type Logger func(format string, args ...any)

func (l Logger) printf(format string, args ...any) {
	if l != nil {
		l(format, args...)
	}
}

// parallel runs fn(i) for i in [0,n) concurrently and returns each call's
// error at its index.
func parallel(n int, fn func(i int) error) []error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = fn(i)
		}(i)
	}
	wg.Wait()
	return errs
}

// project is a ready project loaded for an operation: its record and its
// member sessions, members[i] being the session of rec.Services[i].
type project struct {
	rec     *Record
	members []*waypoint.Manager
}

func load(name string) (*project, error) {
	rec, err := LoadRecord(name)
	if err != nil {
		return nil, err
	}
	if rec.Status != ProjectReady {
		return nil, fmt.Errorf("project %s is %s", name, rec.Status)
	}
	p := &project{rec: rec, members: make([]*waypoint.Manager, len(rec.Services))}
	for i, s := range rec.Services {
		if p.members[i], err = waypoint.LoadManager(s.SessionID); err != nil {
			return nil, fmt.Errorf("service %s: %w", s.Name, err)
		}
	}
	return p, nil
}

// withProjectLock runs fn holding project name's lock, shared or exclusive.
// The project is looked up before the lock is taken, so an unknown name
// leaves no lockfile behind; fn must load it again, since it can have been
// removed while this call waited.
func withProjectLock(name string, exclusive bool, fn func() error) error {
	if _, err := LoadRecord(name); err != nil {
		return err
	}
	if exclusive {
		return waypoint.WithFileLock(projectLockPath(name), fn)
	}
	return waypoint.WithSharedFileLock(projectLockPath(name), fn)
}

// withProject runs fn on the loaded project under its shared lock.
func withProject(name string, fn func(p *project) error) error {
	return withProjectLock(name, false, func() error {
		p, err := load(name)
		if err != nil {
			return err
		}
		return fn(p)
	})
}

// each runs fn on every member in parallel and joins the errors, each named
// by its service.
func (p *project) each(fn func(i int, m *waypoint.Manager) error) error {
	return errors.Join(parallel(len(p.members), func(i int) error {
		if err := fn(i, p.members[i]); err != nil {
			return fmt.Errorf("service %s: %w", p.rec.Services[i].Name, err)
		}
		return nil
	})...)
}

// withReplica runs fn on replica id under its lock. The replica is looked up
// before the lock is taken too, so an unknown ID leaves no lockfile behind.
func (p *project) withReplica(id string, fn func(rep *Replica) error) error {
	name := p.rec.Name
	if _, err := loadReplica(name, id); err != nil {
		return err
	}
	return waypoint.WithFileLock(replicaLockPath(name, id), func() error {
		rep, err := loadReplica(name, id)
		if err != nil {
			return err
		}
		return fn(rep)
	})
}

// --- up ---

// Up creates the project's sessions, starts each service's main fork on
// fabric "main", and runs the services' commands in order. Builds run quietly;
// progress goes to logf. On failure everything created so far is removed.
func Up(spec Spec, logf Logger) (*Record, error) {
	services, subnet, err := plan(spec)
	if err != nil {
		return nil, err
	}
	for _, s := range services {
		if err := waypoint.CheckAttachmentAddress(s.Address); err != nil {
			return nil, fmt.Errorf("service %s: %w", s.Name, err)
		}
	}
	rec := &Record{Name: spec.Name, Subnet: subnet, Services: services, Status: ProjectCreating, CreatedAt: time.Now().Unix()}
	// Exclusive from the claim on, so a cleanup of the half-made project
	// waits for this up to finish or fail.
	err = waypoint.WithFileLock(projectLockPath(spec.Name), func() error { return up(spec, rec, logf) })
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// up claims rec's project and brings it up, under the project lock.
func up(spec Spec, rec *Record, logf Logger) error {
	name, services := rec.Name, rec.Services
	if err := claimProject(rec); err != nil {
		return err
	}
	var recMu sync.Mutex
	saveRec := func() error {
		recMu.Lock()
		defer recMu.Unlock()
		return writeJSON(recordPath(name), rec)
	}

	fail := func(err error) error {
		logf.printf("up failed; removing project %s\n", name)
		_ = cleanup(name, true)
		return err
	}

	// Recorded before the fabric exists, as fork records its replicas, so
	// the cleanup of a failed up finds the fabric and releases it.
	pin := fabricPin(name, MainReplica)
	mainRep := Replica{ID: MainReplica, Fabric: pin, Status: ReplicaCreating, CreatedAt: time.Now().Unix()}
	if err := writeJSON(replicaPath(name, MainReplica), mainRep); err != nil {
		return fail(err)
	}
	if err := waypoint.CreateFabric(pin, false); err != nil {
		return fail(fmt.Errorf("create fabric: %w", err))
	}
	hosts := hostsLines(services)
	commands := make([][]string, len(spec.Services))
	if err := errors.Join(parallel(len(spec.Services), func(i int) error {
		svc := spec.Services[i]
		attach := waypoint.NetworkAttachment{FabricNS: pin, Address: services[i].Address}
		// The session is recorded as soon as it exists, so a failed up
		// removes it.
		created := func(sessionID string) error {
			recMu.Lock()
			rec.Services[i].SessionID = sessionID
			recMu.Unlock()
			logf.printf("starting service %s (session %s)\n", svc.Name, sessionID)
			return saveRec()
		}
		argv, err := startMember(name, svc, attach, hosts, created)
		if err != nil {
			return fmt.Errorf("service %s: %w", svc.Name, err)
		}
		commands[i] = argv
		return nil
	})...); err != nil {
		return fail(err)
	}

	// Commands start in service order, after every shell is up, so a service
	// can resolve and reach the ones before it.
	for i, s := range rec.Services {
		line := startCommand(commands[i])
		if line == "" {
			continue
		}
		rec.Services[i].Command = line
		m, err := waypoint.LoadManager(s.SessionID)
		if err != nil {
			return fail(err)
		}
		logf.printf("running %s: %s\n", s.Name, displayArgv(commands[i]))
		res, err := m.ExecuteForkCommand(waypoint.MainForkID, line)
		if err != nil {
			return fail(fmt.Errorf("start service %s: %w", s.Name, err))
		}
		if res.ExitCode != 0 {
			return fail(fmt.Errorf("start service %s: exit %d: %s", s.Name, res.ExitCode, res.Output))
		}
	}

	mainRep.Status = ReplicaRunning
	if err := writeJSON(replicaPath(name, MainReplica), mainRep); err != nil {
		return fail(err)
	}
	rec.Status = ProjectReady
	if err := saveRec(); err != nil {
		return fail(err)
	}
	return nil
}

// claimProject creates rec's project directory with rec already in it, or
// fails if the project exists. The directory is prepared under a temporary
// name and renamed into place, so a project never exists without its record
// (which cleanup needs to find it); a rename cannot replace a project, whose
// directory is never empty.
func claimProject(rec *Record) error {
	if err := os.MkdirAll(groupsDir(), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(groupsDir(), ".up-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // a no-op once renamed
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(tmp, filepath.Base(recordPath(rec.Name))), rec); err != nil {
		return err
	}
	if err := os.Rename(tmp, projectDir(rec.Name)); err != nil {
		if errors.Is(err, fs.ErrExist) || errors.Is(err, syscall.ENOTEMPTY) {
			return fmt.Errorf("project %s already exists", rec.Name)
		}
		return err
	}
	return nil
}

// startMember creates svc's session and brings its main fork's shell up,
// attached to the project's main fabric. It calls created with the session ID
// once the session exists, and returns the argv the service should run.
func startMember(project string, svc Service, attach waypoint.NetworkAttachment, hosts []string, created func(sessionID string) error) ([]string, error) {
	m, sessionID, err := waypoint.NewManagerWithNetwork(waypoint.NetworkPrivate)
	if err != nil {
		return nil, err
	}
	if err := created(sessionID); err != nil {
		return nil, err
	}
	if err := m.SetGroup(project, svc.Name); err != nil {
		return nil, err
	}
	if err := m.AttachMain(attach); err != nil {
		return nil, err
	}
	if err := m.SetGuestOptions(guestOptions(svc, hosts)); err != nil {
		return nil, err
	}
	if svc.Build != nil {
		if _, _, err := m.BuildEnvironmentFrom(*svc.Build, true); err != nil {
			return nil, err
		}
	} else {
		staged, err := m.StageEnvironment(svc.Rootfs)
		if err != nil {
			return nil, err
		}
		work, err := m.InitEnvironment(staged)
		if err != nil {
			return nil, err
		}
		if _, _, err := m.StartShell(work); err != nil {
			return nil, err
		}
	}
	entrypoint, cmd := m.ImageCommand()
	return serviceArgv(svc, entrypoint, cmd), nil
}

// --- snapshot ---

// Snapshot seals replica into group checkpoint ckpt: the replica's fabric is
// held behind a barrier while every member fork is snapshotted in parallel.
// Without park the replica resumes on ckpt; with park it is released. If a
// member fails after any member began the checkpoint, the manifest is marked
// failed and the replica degraded, and the member checkpoints that succeeded
// are kept; a failure before that leaves the replica running and ckpt free.
func Snapshot(name, replica, ckpt string, park bool) (*Checkpoint, error) {
	if err := waypoint.ValidateID("checkpoint", ckpt); err != nil {
		return nil, err
	}
	if park && replica == MainReplica {
		return nil, fmt.Errorf("cannot park %s: the project needs a live main replica", MainReplica)
	}
	var manifest *Checkpoint
	err := withProject(name, func(p *project) error {
		return p.withReplica(replica, func(rep *Replica) error {
			if rep.Status != ReplicaRunning {
				return fmt.Errorf("replica %s is %s", replica, rep.Status)
			}
			var err error
			if manifest, err = p.claimCheckpoint(rep, ckpt); err != nil {
				return err
			}
			// From here to the last record write, an interrupt would strand
			// the replica behind its barrier with the manifest "creating".
			defer holdSignals()()
			err = p.cut(rep, ckpt, park)
			if err != nil && p.untouched(ckpt) {
				// The replica is as it was: free the ID and leave it running.
				_ = os.Remove(checkpointPath(name, ckpt))
				return err
			}
			return p.commitSnapshot(rep, manifest, park, err)
		})
	})
	return manifest, err
}

// claimCheckpoint reserves ckpt in the project, refusing an ID that the
// project or any member already has, and returns its "creating" manifest.
func (p *project) claimCheckpoint(rep *Replica, ckpt string) (*Checkpoint, error) {
	name := p.rec.Name
	manifest := &Checkpoint{ID: ckpt, Parent: rep.Checkpoint, FromReplica: rep.ID, Status: CheckpointCreating, CreatedAt: time.Now().Unix()}
	err := waypoint.WithFileLock(groupLockPath(name), func() error {
		if _, err := os.Stat(checkpointPath(name, ckpt)); err == nil {
			return fmt.Errorf("checkpoint %s already exists in project %s", ckpt, name)
		}
		for _, s := range p.rec.Services {
			if _, err := waypoint.LoadCheckpointMetadata(s.SessionID, ckpt); err == nil {
				return fmt.Errorf("checkpoint %s already exists in service %s", ckpt, s.Name)
			}
		}
		return writeJSON(checkpointPath(name, ckpt), manifest)
	})
	if err != nil {
		return nil, err
	}
	return manifest, nil
}

// cut snapshots (or parks) every member fork of rep as ckpt behind the fabric
// barrier, which makes the member snapshots one consistent cut. The barrier is
// lowered again however the snapshots end, except after a successful park:
// that replica is gone, and its fabric is released as it is.
func (p *project) cut(rep *Replica, ckpt string, park bool) (err error) {
	if err := waypoint.SetFabricBarrier(rep.Fabric, true); err != nil {
		return fmt.Errorf("raise barrier: %w", err)
	}
	defer func() {
		if park && err == nil {
			return
		}
		if lowerErr := waypoint.SetFabricBarrier(rep.Fabric, false); lowerErr != nil {
			err = errors.Join(err, fmt.Errorf("lower barrier: %w", lowerErr))
		}
	}()
	return p.each(func(_ int, m *waypoint.Manager) error {
		var err error
		if park {
			_, err = m.ParkFork(rep.ID, ckpt)
		} else {
			_, err = m.SnapshotFork(rep.ID, ckpt)
		}
		return err
	})
}

// untouched reports whether no member began checkpoint ckpt. A member records
// the checkpoint before it changes its fork, so a failed cut that left no
// such record anywhere (the barrier could not be raised, or every member
// refused up front) changed nothing.
func (p *project) untouched(ckpt string) bool {
	for _, s := range p.rec.Services {
		if _, err := waypoint.LoadCheckpointMetadata(s.SessionID, ckpt); !errors.Is(err, fs.ErrNotExist) {
			return false
		}
	}
	return true
}

// commitSnapshot records how the cut ended: a failed manifest and a degraded
// replica after cutErr; otherwise a ready manifest and the replica rebased on
// it or, when parked, released.
func (p *project) commitSnapshot(rep *Replica, manifest *Checkpoint, park bool, cutErr error) error {
	name := p.rec.Name
	if cutErr != nil {
		manifest.Status, manifest.Error = CheckpointFailed, cutErr.Error()
		rep.Status = ReplicaDegraded
		_ = writeJSON(replicaPath(name, rep.ID), rep)
		_ = writeJSON(checkpointPath(name, manifest.ID), manifest)
		return cutErr
	}
	manifest.Status = CheckpointReady
	if err := writeJSON(checkpointPath(name, manifest.ID), manifest); err != nil {
		return err
	}
	if park {
		waypoint.ReleaseFabric(rep.Fabric)
		return os.Remove(replicaPath(name, rep.ID))
	}
	rep.Checkpoint = manifest.ID
	return writeJSON(replicaPath(name, rep.ID), rep)
}

// holdSignals keeps SIGINT, SIGTERM, and SIGHUP from killing the process
// until the returned function is called; signals that arrive meanwhile are
// dropped. A member's own child processes still receive a terminal's signal;
// their failure comes back as that member's error.
func holdSignals() (release func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	return func() { signal.Stop(ch) }
}

// --- fork ---

// Fork materializes replicas of group checkpoint ckpt: for each replica a new
// fabric (behind a barrier), then the fork of ckpt in every member, attached
// to it at the member's address; the barrier is lowered once all members are
// restored. ids names the replicas; with ids empty, n replicas get generated
// IDs. A replica that fails is torn down; the others are kept.
func Fork(name, ckpt string, ids []string, n int) ([]Replica, error) {
	var replicas []Replica
	err := withProject(name, func(p *project) (err error) {
		replicas, err = p.fork(ckpt, ids, n)
		return err
	})
	return replicas, err
}

func (p *project) fork(ckpt string, ids []string, n int) ([]Replica, error) {
	name := p.rec.Name
	manifest, err := loadCheckpoint(name, ckpt)
	if err != nil {
		return nil, err
	}
	if manifest.Status != CheckpointReady {
		return nil, fmt.Errorf("checkpoint %s is %s", ckpt, manifest.Status)
	}
	if len(ids) == 0 {
		for i := 0; i < n; i++ {
			id, err := newReplicaID()
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		if err := waypoint.ValidateID("replica", id); err != nil {
			return nil, err
		}
	}

	// Claim every replica ID before any long work.
	if err := waypoint.WithFileLock(groupLockPath(name), func() error {
		for _, id := range ids {
			if _, err := os.Stat(replicaPath(name, id)); err == nil {
				return fmt.Errorf("replica %s already exists", id)
			}
		}
		for _, id := range ids {
			rep := Replica{ID: id, Checkpoint: ckpt, Fabric: fabricPin(name, id), Status: ReplicaCreating, CreatedAt: time.Now().Unix()}
			if err := writeJSON(replicaPath(name, id), rep); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	replicas := make([]Replica, len(ids))
	errs := parallel(len(ids), func(r int) error {
		return p.withReplica(ids[r], func(rep *Replica) error {
			if created, err := p.forkReplica(rep); err != nil {
				_ = p.teardownReplica(rep, created)
				_ = os.Remove(replicaPath(name, rep.ID))
				return fmt.Errorf("replica %s: %w", rep.ID, err)
			}
			replicas[r] = *rep
			return nil
		})
	})
	var ok []Replica
	for r, rep := range replicas {
		if errs[r] == nil {
			ok = append(ok, rep)
		}
	}
	return ok, errors.Join(errs...)
}

// forkReplica materializes rep in every member. created marks each member
// whose fork it made (failed or not), so a rollback removes only those.
func (p *project) forkReplica(rep *Replica) (created []bool, err error) {
	created = make([]bool, len(p.members))
	if err := waypoint.CreateFabric(rep.Fabric, true); err != nil {
		return created, fmt.Errorf("create fabric: %w", err)
	}
	if err := p.each(func(i int, m *waypoint.Manager) error {
		f, err := m.ForkCheckpoint(rep.Checkpoint, waypoint.ForkSpec{
			ID:     rep.ID,
			Attach: &waypoint.NetworkAttachment{FabricNS: rep.Fabric, Address: p.rec.Services[i].Address},
		})
		// A fork record left by a failed restore is ours too; an existing
		// fork with this ID (refused before any record) is not.
		created[i] = f != nil || (err != nil && !errors.Is(err, waypoint.ErrForkExists))
		return err
	}); err != nil {
		return created, err
	}
	if err := waypoint.SetFabricBarrier(rep.Fabric, false); err != nil {
		return created, fmt.Errorf("lower barrier: %w", err)
	}
	rep.Status = ReplicaRunning
	return created, writeJSON(replicaPath(p.rec.Name, rep.ID), rep)
}

// teardownReplica destroys the replica's member forks (only those marked in
// only, if given) and releases its fabric. A member without the fork is fine.
func (p *project) teardownReplica(rep *Replica, only []bool) error {
	err := p.each(func(i int, m *waypoint.Manager) error {
		if only != nil && !only[i] {
			return nil
		}
		if err := m.DestroyFork(rep.ID); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
	waypoint.ReleaseFabric(rep.Fabric)
	return err
}

// --- destroy, exec ---

// Destroy tears down a replica other than main.
func Destroy(name, replica string) error {
	if replica == MainReplica {
		return fmt.Errorf("cannot destroy %s; use suspend or cleanup to end the project's forks", MainReplica)
	}
	return withProject(name, func(p *project) error {
		return p.withReplica(replica, func(rep *Replica) error {
			if err := p.teardownReplica(rep, nil); err != nil {
				return err
			}
			return os.Remove(replicaPath(name, replica))
		})
	})
}

// Exec runs one bash command line in service's fork of replica. It loads only
// that service's session, so it does not depend on the other members.
func Exec(name, replica, service, command string) (*waypoint.ExecResult, error) {
	rec, err := LoadRecord(name)
	if err != nil {
		return nil, err
	}
	if rec.Status != ProjectReady {
		return nil, fmt.Errorf("project %s is %s", name, rec.Status)
	}
	for _, s := range rec.Services {
		if s.Name != service {
			continue
		}
		rep, err := loadReplica(name, replica)
		if err != nil {
			return nil, err
		}
		if rep.Status == ReplicaCreating {
			return nil, fmt.Errorf("replica %s is %s", replica, rep.Status)
		}
		m, err := waypoint.LoadManager(s.SessionID)
		if err != nil {
			return nil, err
		}
		return m.ExecuteForkCommand(replica, command)
	}
	return nil, fmt.Errorf("project %s has no service %q", name, service)
}

// --- suspend, cleanup ---

// Suspend ends every live replica, keeping the project and its checkpoints;
// fork a group checkpoint to continue.
func Suspend(name string) error {
	return withProjectLock(name, true, func() error {
		p, err := load(name)
		if err != nil {
			return err
		}
		err = p.each(func(_ int, m *waypoint.Manager) error { return m.Suspend() })
		return errors.Join(err, releaseReplicas(name))
	})
}

// releaseReplicas releases every replica's fabric and record.
func releaseReplicas(name string) error {
	replicas, err := readAll[Replica](replicasDir(name))
	if err != nil {
		return err
	}
	for _, rep := range replicas {
		waypoint.ReleaseFabric(rep.Fabric)
		_ = os.Remove(replicaPath(name, rep.ID))
	}
	return nil
}

// Cleanup removes the project: every member session, every fabric, and the
// project's records. With force, member sessions are torn down even when
// processes or mounts still hold them. It accepts a project in any state,
// including one a failed up left half-created. When the configuration
// preserves sessions on cleanup, the project that names them is kept too.
func Cleanup(name string, force bool) error {
	return withProjectLock(name, true, func() error { return cleanup(name, force) })
}

// cleanup is Cleanup under the project lock.
func cleanup(name string, force bool) error {
	rec, err := LoadRecord(name)
	if err != nil {
		return err
	}
	errs := parallel(len(rec.Services), func(i int) error {
		s := rec.Services[i]
		if s.SessionID == "" {
			return nil
		}
		m, err := waypoint.LoadManager(s.SessionID)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // already gone
		}
		if err != nil {
			// Unreadable is not gone: without force, keep the record that
			// still names the session.
			return fmt.Errorf("service %s: %w", s.Name, err)
		}
		if force {
			err = m.CleanupForce()
		} else {
			err = m.Cleanup()
		}
		if err != nil {
			return fmt.Errorf("service %s: %w", s.Name, err)
		}
		return nil
	})
	err = errors.Join(append(errs, releaseReplicas(name))...)
	if (err != nil && !force) || waypoint.PreserveSessionOnCleanup {
		return err
	}
	_ = os.RemoveAll(netRuntimeDir(name))
	return errors.Join(err, os.RemoveAll(projectDir(name)))
}
