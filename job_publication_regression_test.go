package main

import "testing"

func TestRuntimePublicationRejectsOlderRevisionOfOwningJob(t *testing.T) {
	app := newIsolatedTestApp(t)
	server, err := app.Deps.ServerInventoryService.Create(Server{Name: "revision-host", Host: "192.0.2.90", User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Deps.ServerState.BeginPackageMutation(server.Name, "updating"); err != nil {
		t.Fatal(err)
	}
	job, err := createServerActionJobWithStateAndManager(app.Deps.CurrentJobManager(), app.Deps.ServerState, jobKindUpdate, server.Name, "review", "", RetryPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	older := job
	older.Revision, older.Status, older.Phase = 1, jobStatusRunning, jobPhasePrechecks
	newer := job
	newer.Revision, newer.Status, newer.Phase = 2, jobStatusSucceeded, jobPhaseComplete
	syncServerStateFromJobRecord(app.Deps.ServerState, newer)
	syncServerStateFromJobRecord(app.Deps.ServerState, older)
	if got := app.Deps.ServerState.CurrentStatusSnapshot(server.Name); got.Status != "done" || got.JobRevision != 2 {
		t.Fatalf("stale revision overwrote completion: %+v", got)
	}
	if _, err := app.Deps.ServerState.BeginPackageMutation(server.Name, "autoremove"); err != nil {
		t.Fatal(err)
	}
	// Old callbacks must also be ignored between admission and job creation.
	syncServerStateFromJobRecord(app.Deps.ServerState, newer)
	if got := app.Deps.ServerState.CurrentStatusSnapshot(server.Name); got.Status != "autoremove" || got.JobID != "" {
		t.Fatalf("old job claimed the next admission: %+v", got)
	}
}
