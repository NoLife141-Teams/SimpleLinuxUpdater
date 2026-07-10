package policies

import (
	"context"
	"errors"
	"testing"
	"time"

	apptimepkg "debian-updater/internal/apptime"
	"debian-updater/internal/servers"
)

func TestCalendarBuildsAllowedAndBlockedWindows(t *testing.T) {
	loc := time.UTC
	svc := NewService(ServiceDeps{
		ListPolicies: func() ([]Policy, error) {
			return []Policy{
				{
					ID:            1,
					Name:          "Nightly",
					Enabled:       true,
					TargetServers: []string{"srv-a"},
					PackageScope:  PackageScopeSecurity,
					UpgradeMode:   UpgradeModeStandard,
					ExecutionMode: ExecutionScanOnly,
					CadenceKind:   CadenceDaily,
					TimeLocal:     "02:00",
				},
				{
					ID:            2,
					Name:          "Weekly",
					Enabled:       true,
					TargetServers: []string{"srv-a"},
					PackageScope:  PackageScopeFull,
					UpgradeMode:   UpgradeModeFull,
					ExecutionMode: ExecutionAutoApply,
					CadenceKind:   CadenceWeekly,
					TimeLocal:     "04:00",
					Weekdays:      []string{"mon"},
				},
			}, nil
		},
		LoadOverrides: func() (map[int64]map[string]bool, error) {
			return map[int64]map[string]bool{}, nil
		},
		LoadGlobalBlackouts: func() ([]BlackoutWindow, error) {
			return []BlackoutWindow{{
				Weekdays:  []string{"sat"},
				StartTime: "23:00",
				EndTime:   "03:00",
			}}, nil
		},
		SnapshotServers: func() []servers.Server {
			return []servers.Server{{Name: "srv-a"}}
		},
		CurrentLocation: func() *time.Location { return loc },
		Now:             func() time.Time { return time.Date(2026, 5, 17, 12, 0, 0, 0, loc) },
	})

	calendar, err := svc.Calendar(CalendarOptions{Days: 2})
	if err != nil {
		t.Fatalf("Calendar() error = %v", err)
	}
	if calendar.StartDate != "2026-05-17" || calendar.EndDate != "2026-05-18" || len(calendar.Policies) != 2 {
		t.Fatalf("calendar = %+v, want two policy calendars over two days", calendar)
	}
	nightly := calendar.Policies[0]
	if nightly.MatchedServers[0] != "srv-a" {
		t.Fatalf("MatchedServers = %+v, want srv-a", nightly.MatchedServers)
	}
	if len(nightly.Days[0].AllowedSlots) != 0 || len(nightly.Days[0].BlockedWindows) != 1 {
		t.Fatalf("Sunday nightly day = %+v, want blocked overnight global window", nightly.Days[0])
	}
	if !nightly.Days[0].BlockedWindows[0].Overnight || !nightly.Days[0].BlockedWindows[0].AppliesToSlot {
		t.Fatalf("blocked window = %+v, want overnight window applying to 02:00 slot", nightly.Days[0].BlockedWindows[0])
	}
	if got := nightly.Days[0].BlockedReasons; len(got) != 1 || got[0] != "global_blackout" {
		t.Fatalf("BlockedReasons = %+v, want global blackout", got)
	}
	if len(nightly.Days[1].AllowedSlots) != 1 || nightly.Days[1].AllowedSlots[0].ScheduledForUTC != "2026-05-18T02:00:00.000000000Z" {
		t.Fatalf("Monday nightly day = %+v, want one allowed slot", nightly.Days[1])
	}

	weekly := calendar.Policies[1]
	if len(weekly.Days[0].AllowedSlots) != 0 {
		t.Fatalf("Sunday weekly day = %+v, want no weekly slot", weekly.Days[0])
	}
	if len(weekly.Days[1].AllowedSlots) != 1 || weekly.Days[1].AllowedSlots[0].PackageScope != PackageScopeFull {
		t.Fatalf("Monday weekly day = %+v, want full-update slot", weekly.Days[1])
	}
}

func TestCalendarUsesApplicationTimeInterpretationForDSTOccurrences(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	applicationTime := apptimepkg.New(apptimepkg.Deps{
		Store: apptimepkg.NewMemoryStore("America/Toronto"),
		Detector: apptimepkg.DetectorFunc(func() (*time.Location, string, error) {
			return loc, loc.String(), nil
		}),
	})
	if err := applicationTime.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	policy := Policy{ID: 1, Name: "DST", Enabled: true, TargetServers: []string{"srv-a"}, PackageScope: PackageScopeSecurity, UpgradeMode: UpgradeModeStandard, ExecutionMode: ExecutionScanOnly, CadenceKind: CadenceDaily, TimeLocal: "02:30"}
	svc := NewService(ServiceDeps{
		ListPolicies:        func() ([]Policy, error) { return []Policy{policy}, nil },
		LoadOverrides:       func() (map[int64]map[string]bool, error) { return map[int64]map[string]bool{}, nil },
		LoadGlobalBlackouts: func() ([]BlackoutWindow, error) { return nil, nil },
		SnapshotServers:     func() []servers.Server { return []servers.Server{{Name: "srv-a"}} },
		CurrentLocation:     func() *time.Location { return loc },
		ApplicationTime:     applicationTime,
		Now:                 func() time.Time { return time.Date(2026, 3, 8, 0, 0, 0, 0, loc) },
	})
	calendar, err := svc.Calendar(CalendarOptions{Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	day := calendar.Policies[0].Days[0]
	if len(day.AllowedSlots) != 0 || len(day.BlockedReasons) != 1 || day.BlockedReasons[0] != "nonexistent_local_time" {
		t.Fatalf("spring-forward day = %+v, want unavailable local occurrence", day)
	}

	policy.TimeLocal = "01:30"
	svc.deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
	svc.deps.Now = func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, loc) }
	calendar, err = svc.Calendar(CalendarOptions{Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	slot := calendar.Policies[0].Days[0].AllowedSlots[0]
	if slot.ScheduledForUTC != "2026-11-01T05:30:00.000000000Z" || slot.TimezoneOffset != "-04:00" {
		t.Fatalf("fall-back slot = %+v, want earlier occurrence", slot)
	}
}

func TestCalendarReportsDSTSlotOffsets(t *testing.T) {
	loc, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	svc := NewService(ServiceDeps{
		ListPolicies: func() ([]Policy, error) {
			return []Policy{{
				ID:            1,
				Name:          "DST",
				Enabled:       true,
				TargetServers: []string{"srv-a"},
				PackageScope:  PackageScopeSecurity,
				UpgradeMode:   UpgradeModeStandard,
				ExecutionMode: ExecutionScanOnly,
				CadenceKind:   CadenceDaily,
				TimeLocal:     "03:30",
			}}, nil
		},
		LoadOverrides:       func() (map[int64]map[string]bool, error) { return map[int64]map[string]bool{}, nil },
		LoadGlobalBlackouts: func() ([]BlackoutWindow, error) { return []BlackoutWindow{}, nil },
		SnapshotServers:     func() []servers.Server { return []servers.Server{{Name: "srv-a"}} },
		CurrentLocation:     func() *time.Location { return loc },
		Now:                 func() time.Time { return time.Date(2026, 3, 7, 12, 0, 0, 0, loc) },
	})

	calendar, err := svc.Calendar(CalendarOptions{Days: 3})
	if err != nil {
		t.Fatalf("Calendar() error = %v", err)
	}
	slots := calendar.Policies[0].Days
	if slots[0].AllowedSlots[0].TimezoneOffset != "-05:00" || slots[1].AllowedSlots[0].TimezoneOffset != "-04:00" {
		t.Fatalf("slot offsets = %s, %s; want DST transition from -05:00 to -04:00", slots[0].AllowedSlots[0].TimezoneOffset, slots[1].AllowedSlots[0].TimezoneOffset)
	}
}

func TestCalendarUnknownPolicyFilter(t *testing.T) {
	svc := NewService(ServiceDeps{
		ListPolicies:        func() ([]Policy, error) { return []Policy{}, nil },
		LoadOverrides:       func() (map[int64]map[string]bool, error) { return map[int64]map[string]bool{}, nil },
		LoadGlobalBlackouts: func() ([]BlackoutWindow, error) { return []BlackoutWindow{}, nil },
	})

	if _, err := svc.Calendar(CalendarOptions{Days: 14, PolicyID: 99}); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("Calendar() error = %v, want ErrPolicyNotFound", err)
	}
}
