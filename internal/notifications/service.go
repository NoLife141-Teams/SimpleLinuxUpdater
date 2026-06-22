package notifications

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	SettingsKey = "notification_hooks_settings"

	EventUpdateComplete     = "update.complete"
	EventScheduleRunFailed  = "schedule.run.failed"
	EventScheduleRunSkipped = "schedule.run.skipped"
	EventBackupRestore      = "backup.restore"
	EventTest               = "notification.test"

	defaultAttempts = 3
)

var supportedEvents = []string{
	EventUpdateComplete,
	EventScheduleRunFailed,
	EventScheduleRunSkipped,
	EventBackupRestore,
}

var errUnsupportedEvent = errors.New("unsupported notification event type")

type DBProvider func() *sql.DB

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type Settings struct {
	Enabled      bool            `json:"enabled"`
	WebhookURL   string          `json:"webhook_url"`
	EventTypes   []string        `json:"event_types"`
	LastDelivery *DeliveryStatus `json:"last_delivery,omitempty"`
}

type SettingsResponse struct {
	Settings
	SupportedEvents []string `json:"supported_events"`
}

type DeliveryStatus struct {
	EventType   string `json:"event_type"`
	Action      string `json:"action"`
	TargetName  string `json:"target_name"`
	Success     bool   `json:"success"`
	Attempts    int    `json:"attempts"`
	StatusCode  int    `json:"status_code,omitempty"`
	Error       string `json:"error,omitempty"`
	DeliveredAt string `json:"delivered_at"`
}

type AuditEvent struct {
	CreatedAt  string
	Actor      string
	Action     string
	TargetType string
	TargetName string
	Status     string
	Message    string
	MetaJSON   string
	ClientIP   string
}

type WebhookPayload struct {
	EventType  string         `json:"event_type"`
	Action     string         `json:"action"`
	Status     string         `json:"status"`
	TargetType string         `json:"target_type"`
	TargetName string         `json:"target_name"`
	Message    string         `json:"message"`
	CreatedAt  string         `json:"created_at"`
	Actor      string         `json:"actor,omitempty"`
	ClientIP   string         `json:"client_ip,omitempty"`
	Meta       map[string]any `json:"meta,omitempty"`
}

type ServiceDeps struct {
	DB         DBProvider
	HTTPClient HTTPClient
	Now        func() time.Time
	Backoff    func(attempt int) time.Duration
	Logf       func(string, ...any)
}

type Service struct {
	deps ServiceDeps
}

func NewService(deps ServiceDeps) *Service {
	if deps.HTTPClient == nil {
		deps.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Backoff == nil {
		deps.Backoff = func(attempt int) time.Duration {
			if attempt <= 1 {
				return 250 * time.Millisecond
			}
			return time.Duration(attempt*attempt) * 250 * time.Millisecond
		}
	}
	if deps.Logf == nil {
		deps.Logf = log.Printf
	}
	return &Service{deps: deps}
}

func SupportedEvents() []string {
	return append([]string(nil), supportedEvents...)
}

func (s *Service) Settings() (SettingsResponse, error) {
	settings, err := s.loadSettings()
	if err != nil {
		return SettingsResponse{}, err
	}
	return SettingsResponse{
		Settings:        settings,
		SupportedEvents: SupportedEvents(),
	}, nil
}

func (s *Service) SaveSettings(settings Settings) (SettingsResponse, error) {
	current, err := s.loadSettings()
	if err != nil {
		return SettingsResponse{}, err
	}
	settings.WebhookURL = strings.TrimSpace(settings.WebhookURL)
	events, err := normalizeEventTypes(settings.EventTypes)
	if err != nil {
		return SettingsResponse{}, err
	}
	settings.EventTypes = events
	settings.LastDelivery = current.LastDelivery
	if settings.Enabled || settings.WebhookURL != "" {
		if err := validateWebhookURL(settings.WebhookURL); err != nil {
			return SettingsResponse{}, err
		}
	}
	if err := s.saveSettings(settings); err != nil {
		return SettingsResponse{}, err
	}
	return SettingsResponse{
		Settings:        settings,
		SupportedEvents: SupportedEvents(),
	}, nil
}

func (s *Service) NotifyAuditEvent(evt AuditEvent) {
	settings, eventType, err := s.notificationPlan(evt, false)
	if err != nil {
		if s.deps.Logf != nil {
			s.deps.Logf("notification webhook delivery skipped for action=%q target=%q: %v", evt.Action, evt.TargetName, err)
		}
		return
	}
	if eventType == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := s.deliverWithSettings(ctx, settings, evt, eventType); err != nil && s.deps.Logf != nil {
			s.deps.Logf("notification webhook delivery skipped/failed for action=%q target=%q: %v", evt.Action, evt.TargetName, err)
		}
	}()
}

func (s *Service) TestDelivery(ctx context.Context) (DeliveryStatus, error) {
	evt := AuditEvent{
		CreatedAt:  s.deps.Now().UTC().Format(time.RFC3339),
		Actor:      "admin",
		Action:     EventTest,
		TargetType: "notification",
		TargetName: "webhook",
		Status:     "test",
		Message:    "Notification test",
		MetaJSON:   `{"source":"admin"}`,
	}
	return s.deliver(ctx, evt, true)
}

func (s *Service) DeliverAuditEvent(ctx context.Context, evt AuditEvent, force bool) (DeliveryStatus, error) {
	return s.deliver(ctx, evt, force)
}

func (s *Service) deliver(ctx context.Context, evt AuditEvent, force bool) (DeliveryStatus, error) {
	settings, eventType, err := s.notificationPlan(evt, force)
	if err != nil || eventType == "" {
		return DeliveryStatus{}, err
	}
	return s.deliverWithSettings(ctx, settings, evt, eventType)
}

func (s *Service) notificationPlan(evt AuditEvent, force bool) (Settings, string, error) {
	settings, err := s.loadSettings()
	if err != nil {
		return Settings{}, "", err
	}
	eventType := strings.TrimSpace(evt.Action)
	if force {
		eventType = EventTest
	} else if !settings.Enabled || !eventEnabled(settings.EventTypes, eventType) {
		return settings, "", nil
	}
	return settings, eventType, nil
}

func (s *Service) deliverWithSettings(ctx context.Context, settings Settings, evt AuditEvent, eventType string) (DeliveryStatus, error) {
	if err := validateWebhookURL(settings.WebhookURL); err != nil {
		return DeliveryStatus{}, err
	}
	payload, err := buildPayload(eventType, evt)
	if err != nil {
		return DeliveryStatus{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return DeliveryStatus{}, err
	}
	status := DeliveryStatus{
		EventType:  eventType,
		Action:     evt.Action,
		TargetName: evt.TargetName,
	}
	var lastErr error
	for attempt := 1; attempt <= defaultAttempts; attempt++ {
		status.Attempts = attempt
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, settings.WebhookURL, bytes.NewReader(body))
		if reqErr != nil {
			lastErr = reqErr
			break
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "SimpleLinuxUpdater/notification-hook")
		resp, doErr := s.deps.HTTPClient.Do(req)
		if doErr != nil {
			lastErr = doErr
		} else {
			status.StatusCode = resp.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				status.Success = true
				status.Error = ""
				status.DeliveredAt = s.deps.Now().UTC().Format(time.RFC3339)
				_ = s.storeLastDelivery(status)
				return status, nil
			}
			lastErr = fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
		}
		if attempt < defaultAttempts {
			select {
			case <-ctx.Done():
				lastErr = ctx.Err()
				attempt = defaultAttempts
			case <-time.After(s.deps.Backoff(attempt)):
			}
		}
	}
	status.Success = false
	status.Error = truncate(strings.TrimSpace(fmt.Sprint(lastErr)), 240)
	status.DeliveredAt = s.deps.Now().UTC().Format(time.RFC3339)
	_ = s.storeLastDelivery(status)
	if lastErr == nil {
		lastErr = errors.New("webhook delivery failed")
	}
	return status, lastErr
}

func (s *Service) loadSettings() (Settings, error) {
	settings := Settings{
		Enabled:    false,
		WebhookURL: "",
		EventTypes: SupportedEvents(),
	}
	if s == nil || s.deps.DB == nil {
		return settings, nil
	}
	db := s.deps.DB()
	if db == nil {
		return settings, nil
	}
	var raw string
	err := db.QueryRow("SELECT value FROM settings WHERE key = ?", SettingsKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return settings, nil
	}
	if err != nil {
		return Settings{}, err
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return Settings{}, err
	}
	if settings.EventTypes == nil {
		settings.EventTypes = SupportedEvents()
	} else {
		events, err := normalizeEventTypes(settings.EventTypes)
		if err != nil {
			events = SupportedEvents()
		}
		settings.EventTypes = events
	}
	settings.WebhookURL = strings.TrimSpace(settings.WebhookURL)
	return settings, nil
}

func (s *Service) saveSettings(settings Settings) error {
	if s == nil || s.deps.DB == nil {
		return nil
	}
	db := s.deps.DB()
	if db == nil {
		return nil
	}
	body, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	_, err = db.Exec(
		"INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		SettingsKey,
		string(body),
	)
	return err
}

func (s *Service) storeLastDelivery(status DeliveryStatus) error {
	settings, err := s.loadSettings()
	if err != nil {
		return err
	}
	settings.LastDelivery = &status
	return s.saveSettings(settings)
}

func buildPayload(eventType string, evt AuditEvent) (WebhookPayload, error) {
	meta := map[string]any{}
	if strings.TrimSpace(evt.MetaJSON) != "" {
		if err := json.Unmarshal([]byte(evt.MetaJSON), &meta); err != nil {
			return WebhookPayload{}, err
		}
	}
	meta = redactMap(meta)
	return WebhookPayload{
		EventType:  eventType,
		Action:     evt.Action,
		Status:     evt.Status,
		TargetType: evt.TargetType,
		TargetName: evt.TargetName,
		Message:    evt.Message,
		CreatedAt:  evt.CreatedAt,
		Actor:      evt.Actor,
		ClientIP:   evt.ClientIP,
		Meta:       meta,
	}, nil
}

func normalizeEventTypes(values []string) ([]string, error) {
	if len(values) == 0 {
		return []string{}, nil
	}
	supported := map[string]struct{}{}
	for _, eventType := range supportedEvents {
		supported[eventType] = struct{}{}
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		eventType := strings.TrimSpace(value)
		if eventType == "" {
			continue
		}
		if _, ok := supported[eventType]; !ok {
			return nil, fmt.Errorf("%w: %s", errUnsupportedEvent, eventType)
		}
		if _, ok := seen[eventType]; ok {
			continue
		}
		seen[eventType] = struct{}{}
		out = append(out, eventType)
	}
	sort.Strings(out)
	return out, nil
}

func eventEnabled(events []string, eventType string) bool {
	for _, candidate := range events {
		if candidate == eventType {
			return true
		}
	}
	return false
}

func validateWebhookURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || parsed.Host == "" {
		return errors.New("webhook_url must be an http or https URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("webhook_url must be an http or https URL")
	}
	return nil
}

func redactMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		if sensitiveKey(key) {
			out[key] = "[redacted]"
			continue
		}
		out[key] = redactValue(value)
	}
	return out
}

func redactValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return redactMap(typed)
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, redactValue(item))
		}
		return out
	default:
		return typed
	}
}

func sensitiveKey(key string) bool {
	clean := strings.ToLower(strings.TrimSpace(key))
	if clean == "" {
		return false
	}
	compact := strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(clean)
	return clean == "key" ||
		clean == "api_key" ||
		clean == "access_key" ||
		clean == "secret_key" ||
		clean == "private_key" ||
		clean == "ssh_key" ||
		compact == "apikey" ||
		compact == "accesskey" ||
		compact == "secretkey" ||
		compact == "privatekey" ||
		compact == "sshkey" ||
		strings.Contains(clean, "password") ||
		strings.Contains(clean, "secret") ||
		strings.Contains(clean, "token") ||
		strings.Contains(compact, "password") ||
		strings.Contains(compact, "secret") ||
		strings.Contains(compact, "token") ||
		strings.HasPrefix(clean, "key_") ||
		strings.HasSuffix(clean, "_key") ||
		strings.HasSuffix(clean, "_secret")
}

func truncate(value string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max])
}
