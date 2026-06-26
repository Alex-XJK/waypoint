package waypoint

import "path/filepath"

func (m *Manager) checkpointsDir() string {
	return filepath.Join(m.baseDir, "checkpoints")
}

func (m *Manager) checkpointDir(checkpointID string) string {
	return filepath.Join(m.checkpointsDir(), checkpointID)
}

func (m *Manager) checkpointUpperDir(checkpointID string) string {
	return filepath.Join(m.checkpointDir(checkpointID), "upper")
}

func (m *Manager) checkpointCriuDir(checkpointID string) string {
	return filepath.Join(m.checkpointDir(checkpointID), "criu")
}

func (m *Manager) locksDir() string {
	return filepath.Join(m.baseDir, "locks")
}

func (m *Manager) sessionLockPath() string {
	return filepath.Join(m.locksDir(), "session.lock")
}

func (m *Manager) forkLockPath(forkID string) string {
	return filepath.Join(m.forkDir(forkID), "lock")
}
