package group

// Project operations. Each fans out over the member sessions in parallel
// through the session API, so per-session locking, configuration, and
// failure handling are the ones every other command uses.
//
// Locking: the project's group lock guards short metadata transitions
// (creating and removing replica and checkpoint records); a replica lock is
// held around the long work on one replica (restores, snapshots, teardown);
// member sessions take their own session and fork locks inside. The group lock
// is never held while a replica lock is taken.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Alex-XJK/waypoint/pkg/waypoint"
)

// Logf, if set, receives progress messages.
var Logf = func(format string, args ...any) {}

// parallel runs fn(i) for i in [0,n) concurrently and joins the errors.
func parallel(n int, fn func(i int) error) error {
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
	return errors.Join(errs...)
}

func members(rec *Record) ([]*waypoint.Manager, error) {
	ms := make([]*waypoint.Manager, len(rec.Services))
	for i, s := range rec.Services {
		if s.SessionID == "" {
			return nil, fmt.Errorf("service %s has no session", s.Name)
		}
		m, err := waypoint.LoadManager(s.SessionID)
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", s.Name, err)
		}
		ms[i] = m
	}
	return ms, nil
}

// --- up ---

// Up creates the project's sessions, starts each service's main fork on
// fabric "main", and runs the services' commands in order. Builds run quietly;
// progress goes to Logf. On failure everything created so far is removed.
func Up(spec Spec) (*Record, error) {
	services, subnet, err := plan(spec)
	if err != nil {
		return nil, err
	}
	for _, s := range services {
		if err := waypoint.CheckAttachmentAddress(s.Address); err != nil {
			return nil, fmt.Errorf("service %s: %w", s.Name, err)
		}
	}
	name := spec.Name
	if err := os.MkdirAll(groupsDir(), 0o755); err != nil {
		return nil, err
	}
	if err := os.Mkdir(projectDir(name), 0o755); err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("project %s already exists", name)
		}
		return nil, err
	}
	rec := &Record{Name: name, Subnet: subnet, Services: services, Status: StatusCreating, CreatedAt: time.Now().Unix()}
	var recMu sync.Mutex
	saveRec := func() error {
		recMu.Lock()
		defer recMu.Unlock()
		return writeJSON(recordPath(name), rec)
	}
	if err := saveRec(); err != nil {
		os.RemoveAll(projectDir(name))
		return nil, err
	}

	fail := func(err error) (*Record, error) {
		Logf("up failed; removing project %s\n", name)
		_ = Cleanup(name, true)
		return nil, err
	}

	pin := fabricPin(name, MainReplica)
	if err := waypoint.CreateFabric(pin, false); err != nil {
		return fail(fmt.Errorf("create fabric: %w", err))
	}
	hosts := hostsLines(services)
	commands := make([][]string, len(spec.Services))
	err = parallel(len(spec.Services), func(i int) error {
		svc := spec.Services[i]
		m, sessionID, err := waypoint.NewManagerWithNetwork(waypoint.NetworkPrivate)
		if err != nil {
			return fmt.Errorf("service %s: %w", svc.Name, err)
		}
		recMu.Lock()
		rec.Services[i].SessionID = sessionID
		recMu.Unlock()
		if err := saveRec(); err != nil {
			return err
		}
		if err := m.SetGroup(name, svc.Name); err != nil {
			return err
		}
		if err := m.AttachMain(waypoint.NetworkAttachment{FabricNS: pin, Address: services[i].Address}); err != nil {
			return fmt.Errorf("service %s: %w", svc.Name, err)
		}
		if err := m.SetGuestOptions(waypoint.GuestOptions{Env: svc.Env, WorkDir: svc.WorkDir, Hostname: svc.Name, Hosts: hosts}); err != nil {
			return fmt.Errorf("service %s: %w", svc.Name, err)
		}
		Logf("starting service %s (session %s)\n", svc.Name, sessionID)
		if svc.Build != nil {
			if _, _, err := m.BuildEnvironmentFrom(*svc.Build, true); err != nil {
				return fmt.Errorf("service %s: %w", svc.Name, err)
			}
		} else {
			staged, err := m.StageEnvironment(svc.Rootfs)
			if err != nil {
				return fmt.Errorf("service %s: %w", svc.Name, err)
			}
			work, err := m.InitEnvironment(staged)
			if err != nil {
				return fmt.Errorf("service %s: %w", svc.Name, err)
			}
			if _, _, err := m.StartShell(work); err != nil {
				return fmt.Errorf("service %s: %w", svc.Name, err)
			}
		}
		entrypoint, cmd := m.ImageCommand()
		commands[i] = serviceArgv(svc, entrypoint, cmd)
		return nil
	})
	if err != nil {
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
		Logf("running %s: %s\n", s.Name, strings.Join(commands[i], " "))
		res, err := m.ExecuteForkCommand(waypoint.MainForkID, line)
		if err != nil {
			return fail(fmt.Errorf("start service %s: %w", s.Name, err))
		}
		if res.ExitCode != 0 {
			return fail(fmt.Errorf("start service %s: exit %d: %s", s.Name, res.ExitCode, res.Output))
		}
	}

	if err := writeJSON(replicaPath(name, MainReplica), Replica{
		ID: MainReplica, Fabric: pin, Status: StatusRunning, CreatedAt: time.Now().Unix(),
	}); err != nil {
		return fail(err)
	}
	rec.Status = StatusReady
	if err := saveRec(); err != nil {
		return fail(err)
	}
	return rec, nil
}

// --- snapshot ---

// Snapshot seals replica into group checkpoint ckpt: the replica's fabric is
// held behind a barrier while every member fork is snapshotted in parallel.
// Without park the replica resumes on ckpt; with park it is released. If any
// member fails, the manifest is marked failed and the replica degraded; the
// member checkpoints that succeeded are kept.
func Snapshot(name, replica, ckpt string, park bool) (*Checkpoint, error) {
	if err := waypoint.ValidateID("checkpoint", ckpt); err != nil {
		return nil, err
	}
	if park && replica == MainReplica {
		return nil, fmt.Errorf("cannot park %s: the project needs a live main replica", MainReplica)
	}
	rec, err := LoadRecord(name)
	if err != nil {
		return nil, err
	}
	ms, err := members(rec)
	if err != nil {
		return nil, err
	}
	var manifest *Checkpoint
	err = waypoint.WithFileLock(replicaLockPath(name, replica), func() error {
		rep, err := loadReplica(name, replica)
		if err != nil {
			return err
		}
		if rep.Status != StatusRunning {
			return fmt.Errorf("replica %s is %s", replica, rep.Status)
		}
		if err := waypoint.WithFileLock(groupLockPath(name), func() error {
			if _, err := os.Stat(checkpointPath(name, ckpt)); err == nil {
				return fmt.Errorf("checkpoint %s already exists in project %s", ckpt, name)
			}
			for _, s := range rec.Services {
				if _, err := waypoint.LoadCheckpointMetadata(s.SessionID, ckpt); err == nil {
					return fmt.Errorf("checkpoint %s already exists in service %s", ckpt, s.Name)
				}
			}
			manifest = &Checkpoint{ID: ckpt, Parent: rep.Checkpoint, FromReplica: replica, Status: StatusCreating, CreatedAt: time.Now().Unix()}
			return writeJSON(checkpointPath(name, ckpt), manifest)
		}); err != nil {
			return err
		}

		if err := waypoint.SetFabricBarrier(rep.Fabric, true); err != nil {
			return fmt.Errorf("raise barrier: %w", err)
		}
		snapErr := parallel(len(ms), func(i int) error {
			var err error
			if park {
				_, err = ms[i].ParkFork(replica, ckpt)
			} else {
				_, err = ms[i].SnapshotFork(replica, ckpt)
			}
			if err != nil {
				return fmt.Errorf("service %s: %w", rec.Services[i].Name, err)
			}
			return nil
		})
		if !park || snapErr != nil {
			if err := waypoint.SetFabricBarrier(rep.Fabric, false); err != nil {
				snapErr = errors.Join(snapErr, fmt.Errorf("lower barrier: %w", err))
			}
		}
		if snapErr != nil {
			manifest.Status, manifest.Error = StatusFailed, snapErr.Error()
			rep.Status = StatusDegraded
			_ = writeJSON(replicaPath(name, replica), rep)
			_ = writeJSON(checkpointPath(name, ckpt), manifest)
			return snapErr
		}
		manifest.Status = StatusReady
		if err := writeJSON(checkpointPath(name, ckpt), manifest); err != nil {
			return err
		}
		if park {
			waypoint.ReleaseFabric(rep.Fabric)
			return os.Remove(replicaPath(name, replica))
		}
		rep.Checkpoint = ckpt
		return writeJSON(replicaPath(name, replica), rep)
	})
	return manifest, err
}

// --- fork ---

// Fork materializes replicas of group checkpoint ckpt: for each replica a new
// fabric (behind a barrier), then the fork of ckpt in every member, attached
// to it at the member's address; the barrier is lowered once all members are
// restored. ids names the replicas; with ids empty, n replicas get generated
// IDs. A replica that fails is torn down; the others are kept.
func Fork(name, ckpt string, ids []string, n int) ([]Replica, error) {
	rec, err := LoadRecord(name)
	if err != nil {
		return nil, err
	}
	manifest, err := loadCheckpoint(name, ckpt)
	if err != nil {
		return nil, err
	}
	if manifest.Status != StatusReady {
		return nil, fmt.Errorf("checkpoint %s is %s", ckpt, manifest.Status)
	}
	ms, err := members(rec)
	if err != nil {
		return nil, err
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
	replicas := make([]Replica, len(ids))
	if err := waypoint.WithFileLock(groupLockPath(name), func() error {
		for i, id := range ids {
			if _, err := os.Stat(replicaPath(name, id)); err == nil {
				return fmt.Errorf("replica %s already exists", id)
			}
			replicas[i] = Replica{ID: id, Checkpoint: ckpt, Fabric: fabricPin(name, id), Status: StatusCreating, CreatedAt: time.Now().Unix()}
		}
		for i := range replicas {
			if err := writeJSON(replicaPath(name, replicas[i].ID), replicas[i]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	errs := make([]error, len(replicas))
	_ = parallel(len(replicas), func(r int) error {
		rep := &replicas[r]
		errs[r] = waypoint.WithFileLock(replicaLockPath(name, rep.ID), func() error {
			created := make([]bool, len(ms))
			if err := forkReplica(rec, ms, rep, created); err != nil {
				_ = teardownReplica(rec, ms, rep, created)
				_ = os.Remove(replicaPath(name, rep.ID))
				return fmt.Errorf("replica %s: %w", rep.ID, err)
			}
			return nil
		})
		return nil
	})
	var ok []Replica
	for i, rep := range replicas {
		if errs[i] == nil {
			ok = append(ok, rep)
		}
	}
	return ok, errors.Join(errs...)
}

// forkReplica materializes rep in every member, marking in created each
// member whose fork it made (failed or not), so a rollback removes only those.
func forkReplica(rec *Record, ms []*waypoint.Manager, rep *Replica, created []bool) error {
	if err := waypoint.CreateFabric(rep.Fabric, true); err != nil {
		return fmt.Errorf("create fabric: %w", err)
	}
	err := parallel(len(ms), func(i int) error {
		f, err := ms[i].ForkCheckpoint(rep.Checkpoint, waypoint.ForkSpec{
			ID:     rep.ID,
			Attach: &waypoint.NetworkAttachment{FabricNS: rep.Fabric, Address: rec.Services[i].Address},
		})
		// A fork record left by a failed restore is ours too; an existing
		// fork with this ID (refused before any record) is not.
		created[i] = f != nil || (err != nil && !errors.Is(err, waypoint.ErrForkExists))
		if err != nil {
			return fmt.Errorf("service %s: %w", rec.Services[i].Name, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := waypoint.SetFabricBarrier(rep.Fabric, false); err != nil {
		return fmt.Errorf("lower barrier: %w", err)
	}
	rep.Status = StatusRunning
	return writeJSON(replicaPath(rec.Name, rep.ID), rep)
}

// teardownReplica destroys the replica's member forks (only those marked in
// only, if given) and releases its fabric. A member without the fork is fine.
func teardownReplica(rec *Record, ms []*waypoint.Manager, rep *Replica, only []bool) error {
	err := parallel(len(ms), func(i int) error {
		if only != nil && !only[i] {
			return nil
		}
		if err := ms[i].DestroyFork(rep.ID); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("service %s: %w", rec.Services[i].Name, err)
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
	rec, err := LoadRecord(name)
	if err != nil {
		return err
	}
	ms, err := members(rec)
	if err != nil {
		return err
	}
	return waypoint.WithFileLock(replicaLockPath(name, replica), func() error {
		rep, err := loadReplica(name, replica)
		if err != nil {
			return err
		}
		if err := teardownReplica(rec, ms, rep, nil); err != nil {
			return err
		}
		return os.Remove(replicaPath(name, replica))
	})
}

// Exec runs one bash command line in service's fork of replica.
func Exec(name, replica, service, command string) (*waypoint.ExecResult, error) {
	rec, err := LoadRecord(name)
	if err != nil {
		return nil, err
	}
	for _, s := range rec.Services {
		if s.Name != service {
			continue
		}
		if _, err := loadReplica(name, replica); err != nil {
			return nil, err
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
	rec, err := LoadRecord(name)
	if err != nil {
		return err
	}
	ms, err := members(rec)
	if err != nil {
		return err
	}
	err = parallel(len(ms), func(i int) error {
		if err := ms[i].Suspend(); err != nil {
			return fmt.Errorf("service %s: %w", rec.Services[i].Name, err)
		}
		return nil
	})
	return errors.Join(err, releaseReplicas(name))
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
// processes or mounts still hold them.
func Cleanup(name string, force bool) error {
	rec, err := LoadRecord(name)
	if err != nil {
		return err
	}
	var (
		mu   sync.Mutex
		errs []error
	)
	_ = parallel(len(rec.Services), func(i int) error {
		s := rec.Services[i]
		if s.SessionID == "" {
			return nil
		}
		m, err := waypoint.LoadManager(s.SessionID)
		if err != nil {
			return nil // never registered, or already gone
		}
		if force {
			err = m.CleanupForce()
		} else {
			err = m.Cleanup()
		}
		if err != nil {
			mu.Lock()
			errs = append(errs, fmt.Errorf("service %s: %w", s.Name, err))
			mu.Unlock()
		}
		return nil
	})
	if err := releaseReplicas(name); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 && !force {
		return errors.Join(errs...)
	}
	if waypoint.PreserveSessionOnCleanup {
		// The member sessions were kept; keep the project that names them.
		Logf("Preserving project %s with its sessions\n", name)
		return errors.Join(errs...)
	}
	_ = os.RemoveAll(netRuntimeDir(name))
	if err := os.RemoveAll(projectDir(name)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
