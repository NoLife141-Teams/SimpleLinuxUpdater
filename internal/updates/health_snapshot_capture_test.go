package updates

import "testing"

func TestHealthSnapshotCaptureCompletionRecordsUpdateObservation(t *testing.T) {
	_, repo := openServerFactsTestRepository(t, "capture-update.db")
	raw := `{"pending_package_count":4,"approved_package_count":3,"approval_scope":"security","security_package_count":0,"precheck_results":[{"name":"disk_space","passed":false,"output":"1000 200","details":"low"}],"postcheck_results":[{"name":"disk_space","passed":true,"output":"2000 800"},{"name":"post_apt_health","passed":true},{"name":"reboot_required","passed":false,"details":"reboot required"}]}`
	if err := repo.CaptureCompletion(MaintenanceCompletion{
		ServerName:  "srv-update",
		CompletedAt: "2026-07-10T12:00:00Z",
		Kind:        MaintenanceKindUpdate,
		Status:      " success ",
		RawJSON:     raw,
	}); err != nil {
		t.Fatalf("CaptureCompletion() error = %v", err)
	}

	got := onlyHealthSnapshot(t, repo, "srv-update")
	if got.PackageCount != 4 || got.SecurityCount != 3 || got.LastUpdateStatus != "success" || got.LastScanStatus != "" {
		t.Fatalf("snapshot counts/status = %+v", got)
	}
	if got.DiskStatus != "ok" || got.DiskFreeKB != 800 || got.DiskTotalKB != 2000 || got.AptStatus != "ok" {
		t.Fatalf("snapshot health = %+v", got)
	}
	if got.RebootRequired == nil || !*got.RebootRequired || got.RawJSON != raw || got.Source != "audit" {
		t.Fatalf("snapshot reboot/raw/source = %+v", got)
	}
}

func TestHealthSnapshotCaptureCompletionRecordsScheduledRunFallbacks(t *testing.T) {
	_, repo := openServerFactsTestRepository(t, "capture-scheduled.db")
	if err := repo.CaptureCompletion(MaintenanceCompletion{
		ServerName:  "srv-scheduled",
		CompletedAt: "2026-07-10T13:00:00Z",
		Kind:        MaintenanceKindScheduledRun,
		Status:      "failure",
		RawJSON:     `{"discovery":{"pending_package_count":6,"security_package_count":2}}`,
	}); err != nil {
		t.Fatalf("CaptureCompletion() error = %v", err)
	}

	got := onlyHealthSnapshot(t, repo, "srv-scheduled")
	if got.PackageCount != 6 || got.SecurityCount != 2 || got.LastScanStatus != "failure" || got.LastUpdateStatus != "" {
		t.Fatalf("snapshot = %+v, want Scheduled Run fallback observation", got)
	}
}

func TestHealthSnapshotCaptureCompletionKeepsUnknownHealthForMalformedMetadata(t *testing.T) {
	_, repo := openServerFactsTestRepository(t, "capture-malformed.db")
	if err := repo.CaptureCompletion(MaintenanceCompletion{ServerName: "srv-bad", Kind: MaintenanceKindUpdate, RawJSON: "{"}); err != nil {
		t.Fatalf("CaptureCompletion() error = %v", err)
	}
	got := onlyHealthSnapshot(t, repo, "srv-bad")
	if got.PackageCount != 0 || got.SecurityCount != 0 || got.DiskStatus != "unknown" || got.AptStatus != "unknown" || got.RawJSON != "{" {
		t.Fatalf("snapshot = %+v, want preserved malformed metadata with unknown health", got)
	}
}

func onlyHealthSnapshot(t *testing.T, repo SQLiteServerFactsRepository, serverName string) HealthSnapshotRecord {
	t.Helper()
	snapshots, err := repo.ListHealthSnapshots("0000", "9999", serverName)
	if err != nil {
		t.Fatalf("ListHealthSnapshots() error = %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("snapshot count = %d, want 1", len(snapshots))
	}
	return snapshots[0]
}
