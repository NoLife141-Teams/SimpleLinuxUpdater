package apptime

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const DisplayLayout = "2006-01-02 15:04:05 MST"

type ChoiceKind string

const (
	ChoiceSystem ChoiceKind = "system"
	ChoiceNamed  ChoiceKind = "named"
	ChoiceOffset ChoiceKind = "offset"
)

type Interpretation struct {
	Configured   ChoiceKind
	Location     *time.Location
	DisplayName  string
	ResolvedName string
	EditableName string
	Diagnostic   string
}

type Store interface {
	Load(context.Context) (string, error)
	Save(context.Context, string) error
}

type Detector interface {
	Detect() (*time.Location, string, error)
}

type DetectorFunc func() (*time.Location, string, error)

func (f DetectorFunc) Detect() (*time.Location, string, error) { return f() }

type Deps struct {
	Store    Store
	Detector Detector
}

type Module struct {
	deps Deps
	mu   sync.RWMutex
	now  Interpretation
}

func New(deps Deps) *Module { return &Module{deps: deps} }

func (m *Module) Initialize(ctx context.Context) error {
	if m == nil || m.deps.Store == nil {
		return errors.New("application time persistence is not configured")
	}
	raw, err := m.deps.Store.Load(ctx)
	if err != nil {
		return err
	}
	interpretation, _, err := m.resolve(raw)
	if err != nil {
		return err
	}
	m.publish(interpretation)
	return nil
}

func (m *Module) Current() Interpretation {
	if m == nil {
		return utcInterpretation("")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.now.Location == nil {
		return utcInterpretation("application time is not initialized")
	}
	return m.now
}

func (m *Module) Configure(ctx context.Context, raw string) (Interpretation, error) {
	if m == nil || m.deps.Store == nil {
		return Interpretation{}, errors.New("application time persistence is not configured")
	}
	interpretation, persisted, err := m.resolve(raw)
	if err != nil {
		return Interpretation{}, err
	}
	if err := m.deps.Store.Save(ctx, persisted); err != nil {
		return Interpretation{}, err
	}
	m.publish(interpretation)
	return interpretation, nil
}

func (m *Module) publish(value Interpretation) {
	m.mu.Lock()
	m.now = value
	m.mu.Unlock()
}

var offsetPattern = regexp.MustCompile(`^([+-])(\d{2}):(\d{2})$`)

func (m *Module) resolve(raw string) (Interpretation, string, error) {
	value := strings.TrimSpace(strings.TrimPrefix(raw, ":"))
	if value == "" || strings.EqualFold(value, "Local") || strings.EqualFold(value, "Server local time") {
		if m.deps.Detector == nil {
			return utcInterpretation("system timezone detector is not configured"), "", nil
		}
		loc, name, detectErr := m.deps.Detector.Detect()
		if loc == nil {
			loc = time.UTC
		}
		name = strings.TrimSpace(name)
		if name == "" || strings.EqualFold(name, "Local") {
			name = locationLabel(loc, time.Now())
		}
		result := Interpretation{Configured: ChoiceSystem, Location: loc, DisplayName: name, ResolvedName: name, EditableName: ""}
		if detectErr != nil {
			result.Diagnostic = detectErr.Error()
		}
		if strings.EqualFold(strings.TrimSpace(raw), "Local") || strings.EqualFold(strings.TrimSpace(raw), "Server local time") {
			result.EditableName = "Local"
		}
		return result, "", nil
	}
	if name, loc, ok := parseOffset(value); ok {
		return Interpretation{Configured: ChoiceOffset, Location: loc, DisplayName: name, ResolvedName: name, EditableName: name}, name, nil
	}
	if !browserSafeName(value) {
		return Interpretation{}, "", fmt.Errorf("invalid timezone %q", value)
	}
	loc, err := time.LoadLocation(value)
	if err != nil {
		return Interpretation{}, "", fmt.Errorf("invalid timezone %q", value)
	}
	name := loc.String()
	return Interpretation{Configured: ChoiceNamed, Location: loc, DisplayName: name, ResolvedName: name, EditableName: name}, name, nil
}

func utcInterpretation(diagnostic string) Interpretation {
	return Interpretation{Configured: ChoiceSystem, Location: time.UTC, DisplayName: "UTC", ResolvedName: "UTC", Diagnostic: diagnostic}
}

func parseOffset(value string) (string, *time.Location, bool) {
	match := offsetPattern.FindStringSubmatch(value)
	if match == nil {
		return "", nil, false
	}
	hours, _ := strconv.Atoi(match[2])
	minutes, _ := strconv.Atoi(match[3])
	if hours > 23 || minutes > 59 {
		return "", nil, false
	}
	offset := hours*3600 + minutes*60
	if match[1] == "-" {
		offset = -offset
	}
	return value, time.FixedZone(value, offset), true
}

func browserSafeName(name string) bool {
	if strings.EqualFold(name, "UTC") || strings.HasPrefix(name, "Etc/") {
		return true
	}
	if strings.HasPrefix(name, "right/") || strings.HasPrefix(name, "posix/") || strings.HasPrefix(name, "SystemV/") {
		return false
	}
	return strings.Contains(name, "/")
}

func locationLabel(loc *time.Location, at time.Time) string {
	if loc == nil {
		return "UTC"
	}
	if name := strings.TrimSpace(loc.String()); name != "" && !strings.EqualFold(name, "Local") {
		return name
	}
	_, offset := at.In(loc).Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	if offset == 0 {
		return "UTC"
	}
	return fmt.Sprintf("%s%02d:%02d", sign, offset/3600, offset%3600/60)
}

func ParseInstant(raw, fallbackLayout string) (time.Time, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return time.Time{}, errors.New("timestamp is required")
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, fallbackLayout} {
		if layout == "" {
			continue
		}
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported timestamp format %q", value)
}

func (i Interpretation) Format(raw, fallbackLayout string) (string, string) {
	loc := i.Location
	if loc == nil {
		loc = time.UTC
	}
	parsed, err := ParseInstant(raw, fallbackLayout)
	if err != nil {
		if strings.TrimSpace(raw) == "" {
			return "-", i.DisplayName
		}
		return strings.TrimSpace(raw), i.DisplayName
	}
	return parsed.In(loc).Format(DisplayLayout), i.DisplayName
}

type OccurrenceKind string

const (
	OccurrenceValid       OccurrenceKind = "valid"
	OccurrenceNonexistent OccurrenceKind = "nonexistent"
	OccurrenceAmbiguous   OccurrenceKind = "ambiguous"
)

type Occurrence struct {
	Kind     OccurrenceKind
	Instant  time.Time
	Offset   string
	LocalDay string
}

func (i Interpretation) ResolveLocal(day time.Time, hour, minute int) Occurrence {
	loc := i.Location
	if loc == nil {
		loc = time.UTC
	}
	year, month, date := day.In(loc).Date()
	offsets := map[int]struct{}{}
	probe := time.Date(year, month, date, hour, minute, 0, 0, loc)
	for _, sample := range []time.Time{probe.Add(-36 * time.Hour), probe.Add(-12 * time.Hour), probe, probe.Add(12 * time.Hour), probe.Add(36 * time.Hour)} {
		_, offset := sample.In(loc).Zone()
		offsets[offset] = struct{}{}
	}
	matches := make([]time.Time, 0, 2)
	wallUTC := time.Date(year, month, date, hour, minute, 0, 0, time.UTC)
	for offset := range offsets {
		candidate := wallUTC.Add(-time.Duration(offset) * time.Second)
		local := candidate.In(loc)
		if local.Year() == year && local.Month() == month && local.Day() == date && local.Hour() == hour && local.Minute() == minute {
			matches = append(matches, candidate)
		}
	}
	sort.Slice(matches, func(a, b int) bool { return matches[a].Before(matches[b]) })
	result := Occurrence{Kind: OccurrenceNonexistent, LocalDay: fmt.Sprintf("%04d-%02d-%02d", year, month, date)}
	if len(matches) == 0 {
		return result
	}
	result.Kind = OccurrenceValid
	if len(matches) > 1 {
		result.Kind = OccurrenceAmbiguous
	}
	result.Instant = matches[0]
	_, offset := matches[0].In(loc).Zone()
	result.Offset = offsetLabel(offset)
	return result
}

func offsetLabel(offset int) string {
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	return fmt.Sprintf("%s%02d:%02d", sign, offset/3600, offset%3600/60)
}

type MemoryStore struct {
	mu        sync.Mutex
	value     string
	LoadError error
	SaveError error
}

func NewMemoryStore(value string) *MemoryStore { return &MemoryStore{value: value} }

func (s *MemoryStore) Load(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value, s.LoadError
}

func (s *MemoryStore) Save(_ context.Context, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.SaveError != nil {
		return s.SaveError
	}
	s.value = value
	return nil
}
