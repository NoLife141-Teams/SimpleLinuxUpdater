package policies

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apptimepkg "debian-updater/internal/apptime"
	"debian-updater/internal/servers"
)

type schedulerRecoverySnapshot struct {
	Policies        []Policy
	Overrides       map[int64]map[string]bool
	GlobalBlackouts []BlackoutWindow
	Servers         []servers.Server
	Location        *time.Location
}

type schedulerRecoveryTimeStore struct{}

func (schedulerRecoveryTimeStore) Load(context.Context) (string, error) { return "", nil }
func (schedulerRecoveryTimeStore) Save(context.Context, string) error  { return nil }

func (s *Service) captureSchedulerRecoverySnapshot() (schedulerRecoverySnapshot, error) {
	deps := s.EnsureDeps()
	if deps.ListPolicies == nil || deps.LoadOverrides == nil || deps.LoadGlobalBlackouts == nil || deps.SnapshotServers == nil {
		return schedulerRecoverySnapshot{}, errors.New("policy service dependencies are incomplete")
	}
	policies, err := deps.ListPolicies()
	if err != nil {
		return schedulerRecoverySnapshot{}, err
	}
	overrides, err := deps.LoadOverrides()
	if err != nil {
		return schedulerRecoverySnapshot{}, err
	}
	globalBlackouts, err := deps.LoadGlobalBlackouts()
	if err != nil {
		return schedulerRecoverySnapshot{}, err
	}
	loc := deps.CurrentLocation()
	if loc == nil {
		loc = time.Local
	}
	return schedulerRecoverySnapshot{
		Policies:        cloneRecoveryPolicies(policies),
		Overrides:       cloneRecoveryOverrides(overrides),
		GlobalBlackouts: cloneRecoveryBlackouts(globalBlackouts),
		Servers:         cloneRecoveryServers(deps.SnapshotServers()),
		Location:        loc,
	}, nil
}

func (snapshot schedulerRecoverySnapshot) bind(service *Service) *Service {
	deps := service.EnsureDeps()
	policies := cloneRecoveryPolicies(snapshot.Policies)
	overrides := cloneRecoveryOverrides(snapshot.Overrides)
	blackouts := cloneRecoveryBlackouts(snapshot.GlobalBlackouts)
	serverSnapshot := cloneRecoveryServers(snapshot.Servers)
	loc := snapshot.Location
	if loc == nil {
		loc = time.Local
	}

	deps.ListPolicies = func() ([]Policy, error) {
		return cloneRecoveryPolicies(policies), nil
	}
	deps.LoadOverrides = func() (map[int64]map[string]bool, error) {
		return cloneRecoveryOverrides(overrides), nil
	}
	deps.LoadGlobalBlackouts = func() ([]BlackoutWindow, error) {
		return cloneRecoveryBlackouts(blackouts), nil
	}
	deps.SnapshotServers = func() []servers.Server {
		return cloneRecoveryServers(serverSnapshot)
	}
	deps.CurrentLocation = func() *time.Location { return loc }
	deps.ApplicationTime = fixedRecoveryApplicationTime(loc)
	return NewService(deps)
}

func fixedRecoveryApplicationTime(loc *time.Location) *apptimepkg.Module {
	if loc == nil {
		loc = time.Local
	}
	module := apptimepkg.New(apptimepkg.Deps{
		Store: schedulerRecoveryTimeStore{},
		Detector: apptimepkg.DetectorFunc(func() (*time.Location, string, error) {
			return loc, loc.String(), nil
		}),
	})
	if err := module.Initialize(context.Background()); err != nil {
		return nil
	}
	return module
}

func (snapshot schedulerRecoverySnapshot) fingerprint() (string, error) {
	overrideState := make([]schedulerStateOverride, 0)
	for policyID, perPolicy := range snapshot.Overrides {
		for serverName, disabled := range perPolicy {
			overrideState = append(overrideState, schedulerStateOverride{
				PolicyID:   policyID,
				ServerName: strings.TrimSpace(serverName),
				Disabled:   disabled,
			})
		}
	}
	sort.Slice(overrideState, func(i, j int) bool {
		if overrideState[i].PolicyID != overrideState[j].PolicyID {
			return overrideState[i].PolicyID < overrideState[j].PolicyID
		}
		left := strings.ToLower(overrideState[i].ServerName)
		right := strings.ToLower(overrideState[j].ServerName)
		if left == right {
			return overrideState[i].ServerName < overrideState[j].ServerName
		}
		return left < right
	})

	serverState := make([]schedulerStateServer, 0, len(snapshot.Servers))
	for _, server := range snapshot.Servers {
		serverState = append(serverState, schedulerStateServer{
			Name: strings.TrimSpace(server.Name),
			Tags: NormalizeStringList(server.Tags),
		})
	}
	sort.Slice(serverState, func(i, j int) bool {
		left := strings.ToLower(serverState[i].Name)
		right := strings.ToLower(serverState[j].Name)
		if left == right {
			return serverState[i].Name < serverState[j].Name
		}
		return left < right
	})

	locationName := ""
	if snapshot.Location != nil {
		locationName = snapshot.Location.String()
	}
	payload := schedulerStateFingerprintPayload{
		Policies:        cloneRecoveryPolicies(snapshot.Policies),
		Overrides:       overrideState,
		GlobalBlackouts: cloneRecoveryBlackouts(snapshot.GlobalBlackouts),
		Servers:         serverState,
		Location:        locationName,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest[:]), nil
}

func cloneRecoveryPolicies(in []Policy) []Policy {
	out := append([]Policy(nil), in...)
	for i := range out {
		out[i].IncludeTags = append([]string(nil), in[i].IncludeTags...)
		out[i].ExcludeTags = append([]string(nil), in[i].ExcludeTags...)
		out[i].TargetServers = append([]string(nil), in[i].TargetServers...)
		out[i].Weekdays = append([]string(nil), in[i].Weekdays...)
		out[i].PolicyBlackouts = cloneRecoveryBlackouts(in[i].PolicyBlackouts)
		out[i].MatchedServers = append([]string(nil), in[i].MatchedServers...)
	}
	return out
}

func cloneRecoveryOverrides(in map[int64]map[string]bool) map[int64]map[string]bool {
	out := make(map[int64]map[string]bool, len(in))
	for policyID, perPolicy := range in {
		copyPerPolicy := make(map[string]bool, len(perPolicy))
		for serverName, disabled := range perPolicy {
			copyPerPolicy[serverName] = disabled
		}
		out[policyID] = copyPerPolicy
	}
	return out
}

func cloneRecoveryBlackouts(in []BlackoutWindow) []BlackoutWindow {
	out := append([]BlackoutWindow(nil), in...)
	for i := range out {
		out[i].Weekdays = append([]string(nil), in[i].Weekdays...)
	}
	return out
}

func cloneRecoveryServers(in []servers.Server) []servers.Server {
	out := append([]servers.Server(nil), in...)
	for i := range out {
		out[i].Tags = append([]string(nil), in[i].Tags...)
	}
	return out
}
