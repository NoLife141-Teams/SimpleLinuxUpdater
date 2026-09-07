package servers

// BindActionJob attaches persistence to the admission that created it. A late
// creation or publication cannot take ownership from a newer admission.
func (s *State) BindActionJob(name, id string, generation uint64) bool {
	s.Lock()
	defer s.Unlock()
	status := (*s.statusMap)[name]
	if status == nil || status.ActionGeneration != generation || (status.ActionRunning && status.JobID != id) {
		return false
	}
	status.JobID = id
	status.JobRevision = -1
	return true
}

// BeginActionRunner keeps admission closed through terminal persistence,
// runtime publication, session closure and audit recording.
func (s *State) BeginActionRunner(name, id string) (uint64, bool) {
	s.Lock()
	defer s.Unlock()
	status := (*s.statusMap)[name]
	if status == nil || status.ActionRunning || (status.JobID != id && status.ActionGeneration != 0) {
		return 0, false
	}
	if status.JobID != id {
		status.JobRevision = -1
	}
	status.JobID = id
	status.ActionRunning = true
	return status.ActionGeneration, true
}

func (s *State) FinishActionRunner(name, id string, generation uint64) {
	s.Lock()
	defer s.Unlock()
	status := (*s.statusMap)[name]
	if status != nil && status.JobID == id && status.ActionGeneration == generation {
		status.ActionRunning = false
	}
}
