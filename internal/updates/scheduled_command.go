package updates

import "encoding/json"

// Persisted job metadata remains the source of scheduling context for the same
// command runner used by manual and scheduled cleanup.
func (r *withActorRunner) addScheduledCommandAuditMeta(meta map[string]any) {
	jm := r.deps().CurrentJobManager()
	if jm == nil || r.jobID == "" {
		return
	}
	job, err := jm.GetJob(r.jobID)
	if err != nil {
		return
	}
	var scheduled ScheduledJobMeta
	if json.Unmarshal([]byte(job.MetaJSON), &scheduled) != nil || scheduled.Trigger != "scheduled" {
		return
	}
	meta["trigger"] = scheduled.Trigger
	meta["run_id"] = scheduled.RunID
	meta["policy_id"] = scheduled.PolicyID
	meta["policy_name"] = scheduled.PolicyName
	meta["scheduled_for_utc"] = scheduled.ScheduledFor
	meta["execution_mode"] = scheduled.ExecutionMode
}
