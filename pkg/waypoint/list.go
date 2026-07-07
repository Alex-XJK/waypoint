package waypoint

// Session inspection for humans and machine callers (StateFork etc.).

import "sort"

// SessionListing is the stable JSON shape emitted by `waypoint list --json`.
// Checkpoints are the immutable DAG nodes; Forks are the live instances.
type SessionListing struct {
	SessionID   string     `json:"session_id"`
	Checkpoints []Metadata `json:"checkpoints"`
	Forks       []*Fork    `json:"forks"`
}

// ListSession collects checkpoint metadata and live fork records.
func (m *Manager) ListSession() (*SessionListing, error) {
	ids, err := m.ListCheckpoints()
	if err != nil {
		return nil, err
	}
	sort.Strings(ids)

	listing := &SessionListing{
		SessionID:   m.sessionID,
		Checkpoints: make([]Metadata, 0, len(ids)),
	}
	for _, id := range ids {
		metadata, err := m.loadMetadata(id)
		if err != nil {
			continue // e.g. torn write during concurrent snapshot; skip
		}
		listing.Checkpoints = append(listing.Checkpoints, *metadata)
	}

	forks, err := m.ListForks()
	if err != nil {
		return nil, err
	}
	sort.Slice(forks, func(i, j int) bool { return forks[i].ID < forks[j].ID })
	listing.Forks = forks
	return listing, nil
}
