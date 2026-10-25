package jobs

import (
	"encoding/json"
	"errors"
)

// MergeActiveMetadata applies named facts to the latest active job with CAS retry.
// It preserves metadata owned by other lifecycles and never revives a terminal job.
func (m *Manager) MergeActiveMetadata(id string, patch map[string]any) error {
	if m == nil || m.repo == nil {
		return errors.New("job manager unavailable")
	}
	for attempt := 0; attempt < 8; attempt++ {
		current, err := m.repo.Get(id)
		if err != nil {
			return err
		}
		if isTerminalStatus(current.Status) {
			return ErrTransitionConflict
		}
		meta := map[string]any{}
		if current.MetaJSON != "" {
			if err = json.Unmarshal([]byte(current.MetaJSON), &meta); err != nil {
				return err
			}
		}
		if meta == nil {
			meta = map[string]any{}
		}
		for key, value := range patch {
			meta[key] = value
		}
		raw, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		encoded := string(raw)
		next, _, err := m.applyIntent(current, Intent{Kind: IntentReplaceMetadata, MetaJSON: &encoded})
		if err != nil {
			return err
		}
		updated, err := m.repo.ApplyTransition(next, current.Revision, true)
		if err != nil {
			return err
		}
		if !updated {
			continue
		}
		m.syncRuntime(id)
		m.notify("job.update")
		return nil
	}
	return ErrTransitionConflict
}
