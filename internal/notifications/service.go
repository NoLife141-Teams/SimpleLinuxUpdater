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
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	SettingsKey = "notification_hooks_settings"

	EventUpdateComplete     = "update.complete"
	EventScheduleRunFailed  = "schedule.run.failed"
	EventScheduleRunSkipped = "schedule.run.skipped"
	EventBackupRestore      = "backup.restore"
	EventTest               = "notification.test"

	defaultAttempts  = 3
	defaultQueueSize = 64
)

var supportedEvents = []string{
	EventUpdateComplete,
	EventScheduleRunFailed,
	EventScheduleRunSkipped,
	EventBackupRestore,
}

var errUnsupportedEvent = errors.New("unsupported notification event type")

type WebhookURLIntent string

const (
	WebhookURLPreserve WebhookURLIntent = "preserve"
	WebhookURLReplace  WebhookURLIntent = "replace"
	WebhookURLClear    WebhookURLIntent = "clear"
)

type DBProvider func() *sql.DB

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type Settings struct {
	Enabled      bool
	WebhookURL   string
	EventTypes   []string
	LastDelivery *DeliveryStatus
}

type SettingsResponse struct {
	Enabled           bool             `json:"enabled"`
	WebhookConfigured bool             `json:"webhook_configured"`
	WebhookURLMasked  string           `json:"webhook_url_masked,omitempty"`
	WebhookURLIntent  WebhookURLIntent `json:"webhook_url_intent"`
	EventTypes        []string         `json:"event_types"`
	LastDelivery      *DeliveryStatus  `json:"last_delivery,omitempty"`
	SupportedEvents   []string         `json:"supported_events"`
}

type SettingsUpdate struct {
	Enabled          bool             `json:"enabled"`
	WebhookURL       string           `json:"webhook_url,omitempty"`
	WebhookURLIntent WebhookURLIntent `json:"webhook_url_intent,omitempty"`
	EventTypes       []string         `json:"event_types"`
}

type persistedSettings struct {
	Enabled             bool            `json:"enabled"`
	LegacyWebhookURL    string          `json:"webhook_url,omitempty"`
	EncryptedWebhookURL string          `json:"webhook_url_enc,omitempty"`
	EventTypes          []string        `json:"event_types"`
	LastDelivery        *DeliveryStatus `json:"last_delivery,omitempty"`
}

type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
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

type DeliveryIntent struct {
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

type AdmissionState string

const (
	AdmissionAdmitted AdmissionState = "admitted"
	AdmissionSkipped  AdmissionState = "skipped"
	AdmissionRejected AdmissionState = "rejected"
	AdmissionClosing  AdmissionState = "closing"
)

type Admission struct {
	State AdmissionState
	Error string
}

type Lifecycle interface {
	Settings() (SettingsResponse, error)
	SaveSettings(SettingsUpdate) (SettingsResponse, error)
	Accept(DeliveryIntent) Admission
	TestDelivery(context.Context) (DeliveryStatus, error)
	Close(context.Context) error
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
	DB              DBProvider
	HTTPClient      HTTPClient
	EncryptSecret   func(string) (string, error)
	DecryptSecret   func(string) (string, error)
	Now             func() time.Time
	Backoff         func(attempt int) time.Duration
	Logf            func(string, ...any)
	QueueSize       int
	DeliveryTimeout time.Duration
}

type Service struct {
	deps ServiceDeps

	settingsMu  sync.Mutex
	lifecycleMu sync.Mutex
	queue       chan queuedDelivery
	closing     bool
	cancel      context.CancelFunc
	done        chan struct{}
}

type queuedDelivery struct {
	settings  Settings
	intent    DeliveryIntent
	eventType string
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
	if deps.QueueSize <= 0 {
		deps.QueueSize = defaultQueueSize
	}
	if deps.DeliveryTimeout <= 0 {
		deps.DeliveryTimeout = 30 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		deps:   deps,
		queue:  make(chan queuedDelivery, deps.QueueSize),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go s.run(ctx)
	return s
}

func (s *Service) run(ctx context.Context) {
	defer close(s.done)
	for {
		select {
		case <-ctx.Done():
			return
		case delivery, ok := <-s.queue:
			if !ok {
				return
			}
			if ctx.Err() != nil {
				return
			}
			deliveryCtx, cancel := context.WithTimeout(ctx, s.deps.DeliveryTimeout)
			_, err := s.deliverWithSettings(deliveryCtx, delivery.settings, delivery.intent, delivery.eventType)
			cancel()
			if err != nil && s.deps.Logf != nil {
				s.deps.Logf("notification delivery failed for action=%q target=%q: %v", delivery.intent.Action, delivery.intent.TargetName, err)
			}
		}
	}
}

func (s *Service) Accept(intent DeliveryIntent) Admission {
	s.lifecycleMu.Lock()
	if s.closing {
		s.lifecycleMu.Unlock()
		return Admission{State: AdmissionClosing}
	}
	s.lifecycleMu.Unlock()

	settings, eventType, err := s.notificationPlan(intent, false)
	if err != nil {
		return Admission{State: AdmissionRejected, Error: err.Error()}
	}
	if eventType == "" {
		return Admission{State: AdmissionSkipped}
	}
	delivery := queuedDelivery{settings: settings, intent: intent, eventType: eventType}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closing {
		return Admission{State: AdmissionClosing}
	}
	select {
	case s.queue <- delivery:
		return Admission{State: AdmissionAdmitted}
	default:
		return Admission{State: AdmissionRejected, Error: "notification delivery queue is full"}
	}
}

func (s *Service) Close(ctx context.Context) error {
	s.lifecycleMu.Lock()
	if !s.closing {
		s.closing = true
		close(s.queue)
	}
	done := s.done
	s.lifecycleMu.Unlock()
	select {
	case <-done:
		s.cancel()
		return nil
	case <-ctx.Done():
		s.cancel()
		return ctx.Err()
	}
}

func SupportedEvents() []string {
	return append([]string(nil), supportedEvents...)
}

func (s *Service) Settings() (SettingsResponse, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	settings, err := s.loadSettings()
	if err != nil {
		return SettingsResponse{}, err
	}
	return settingsResponse(settings, WebhookURLPreserve), nil
}

func (s *Service) SaveSettings(update SettingsUpdate) (SettingsResponse, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	current, err := s.loadSettings()
	if err != nil {
		return SettingsResponse{}, err
	}
	events, err := normalizeEventTypes(update.EventTypes)
	if err != nil {
		return SettingsResponse{}, err
	}
	intent, err := normalizeWebhookURLIntent(update)
	if err != nil {
		return SettingsResponse{}, err
	}
	settings := Settings{
		Enabled:      update.Enabled,
		WebhookURL:   current.WebhookURL,
		EventTypes:   events,
		LastDelivery: current.LastDelivery,
	}
	switch intent {
	case WebhookURLReplace:
		replacement := strings.TrimSpace(update.WebhookURL)
		if replacement == "" {
			return SettingsResponse{}, validationError("A replacement webhook URL is required.")
		}
		if err := validateReplacementWebhookURL(replacement); err != nil {
			return SettingsResponse{}, err
		}
		settings.WebhookURL = replacement
	case WebhookURLClear:
		if strings.TrimSpace(update.WebhookURL) != "" {
			return SettingsResponse{}, validationError("webhook_url must be empty when clearing the configured URL.")
		}
		if update.Enabled {
			return SettingsResponse{}, validationError("Disable webhook delivery before clearing the configured URL.")
		}
		settings.WebhookURL = ""
	case WebhookURLPreserve:
		if strings.TrimSpace(update.WebhookURL) != "" {
			return SettingsResponse{}, validationError("webhook_url must be empty when preserving the configured URL.")
		}
	}
	if settings.Enabled && settings.WebhookURL == "" {
		return SettingsResponse{}, validationError("Enablement requires a configured webhook URL. Choose Replace URL first.")
	}
	if err := s.saveSettings(settings); err != nil {
		return SettingsResponse{}, err
	}
	return settingsResponse(settings, intent), nil
}

func (s *Service) TestDelivery(ctx context.Context) (DeliveryStatus, error) {
	evt := DeliveryIntent{
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

func (s *Service) deliver(ctx context.Context, evt DeliveryIntent, force bool) (DeliveryStatus, error) {
	settings, eventType, err := s.notificationPlan(evt, force)
	if err != nil || eventType == "" {
		return DeliveryStatus{}, err
	}
	return s.deliverWithSettings(ctx, settings, evt, eventType)
}

func (s *Service) notificationPlan(evt DeliveryIntent, force bool) (Settings, string, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
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

func (s *Service) deliverWithSettings(ctx context.Context, settings Settings, evt DeliveryIntent, eventType string) (DeliveryStatus, error) {
	if err := validateStoredWebhookURL(settings.WebhookURL); err != nil {
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
			lastErr = errors.New("webhook request could not be created")
			break
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "SimpleLinuxUpdater/notification-hook")
		resp, doErr := s.deps.HTTPClient.Do(req)
		if doErr != nil {
			lastErr = safeWebhookRequestError(doErr)
		} else {
			status.StatusCode = resp.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				status.Success = true
				status.Error = ""
				status.DeliveredAt = s.deps.Now().UTC().Format(time.RFC3339)
				if err := s.storeLastDelivery(status); err != nil {
					return status, fmt.Errorf("record notification delivery outcome: %w", err)
				}
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
	status.Error = safeDeliveryError(lastErr)
	status.DeliveredAt = s.deps.Now().UTC().Format(time.RFC3339)
	if err := s.storeLastDelivery(status); err != nil {
		if lastErr == nil {
			lastErr = errors.New("webhook delivery failed")
		}
		return status, fmt.Errorf("%v; record notification delivery outcome: %w", lastErr, err)
	}
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
	var stored persistedSettings
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return Settings{}, err
	}
	settings.Enabled = stored.Enabled
	settings.EventTypes = stored.EventTypes
	settings.LastDelivery = safeDeliveryStatus(stored.LastDelivery)
	legacyURLStored := strings.TrimSpace(stored.EncryptedWebhookURL) == "" &&
		strings.TrimSpace(stored.LegacyWebhookURL) != ""
	switch {
	case strings.TrimSpace(stored.EncryptedWebhookURL) != "":
		if s.deps.DecryptSecret == nil {
			return Settings{}, errors.New("protected webhook URL cannot be loaded")
		}
		decrypted, err := s.deps.DecryptSecret(stored.EncryptedWebhookURL)
		if err != nil {
			return Settings{}, errors.New("protected webhook URL cannot be loaded")
		}
		settings.WebhookURL = strings.TrimSpace(decrypted)
	default:
		settings.WebhookURL = strings.TrimSpace(stored.LegacyWebhookURL)
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
	if legacyURLStored {
		if err := s.saveSettings(settings); err != nil {
			return Settings{}, errors.New("legacy webhook URL could not be protected")
		}
	}
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
	stored := persistedSettings{
		Enabled:      settings.Enabled,
		EventTypes:   append([]string(nil), settings.EventTypes...),
		LastDelivery: safeDeliveryStatus(settings.LastDelivery),
	}
	if strings.TrimSpace(settings.WebhookURL) != "" {
		if s.deps.EncryptSecret == nil {
			return errors.New("webhook URL protection is unavailable")
		}
		encrypted, err := s.deps.EncryptSecret(settings.WebhookURL)
		if err != nil {
			return errors.New("webhook URL could not be protected")
		}
		stored.EncryptedWebhookURL = encrypted
	}
	body, err := json.Marshal(stored)
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
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	settings, err := s.loadSettings()
	if err != nil {
		return err
	}
	settings.LastDelivery = &status
	return s.saveSettings(settings)
}

func ReencryptStoredWebhookURL(
	ctx context.Context,
	tx *sql.Tx,
	decrypt func(string) (string, error),
	encrypt func(string) (string, error),
) error {
	if tx == nil {
		return errors.New("notification settings transaction is unavailable")
	}
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", SettingsKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return errors.New("read restored notification settings")
	}
	var stored persistedSettings
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return errors.New("parse restored notification settings")
	}
	webhookURL := strings.TrimSpace(stored.LegacyWebhookURL)
	if strings.TrimSpace(stored.EncryptedWebhookURL) != "" {
		if decrypt == nil {
			return errors.New("notification webhook decryption is unavailable")
		}
		webhookURL, err = decrypt(stored.EncryptedWebhookURL)
		if err != nil {
			return errors.New("decrypt restored notification webhook")
		}
	}
	stored.LegacyWebhookURL = ""
	stored.EncryptedWebhookURL = ""
	if strings.TrimSpace(webhookURL) != "" {
		if encrypt == nil {
			return errors.New("notification webhook encryption is unavailable")
		}
		stored.EncryptedWebhookURL, err = encrypt(webhookURL)
		if err != nil {
			return errors.New("encrypt restored notification webhook")
		}
	}
	body, err := json.Marshal(stored)
	if err != nil {
		return errors.New("encode restored notification settings")
	}
	if _, err := tx.ExecContext(ctx, "UPDATE settings SET value = ? WHERE key = ?", string(body), SettingsKey); err != nil {
		return errors.New("update restored notification settings")
	}
	return nil
}

func buildPayload(eventType string, evt DeliveryIntent) (WebhookPayload, error) {
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

func normalizeWebhookURLIntent(update SettingsUpdate) (WebhookURLIntent, error) {
	intent := WebhookURLIntent(strings.ToLower(strings.TrimSpace(string(update.WebhookURLIntent))))
	if intent == "" {
		if strings.TrimSpace(update.WebhookURL) != "" {
			return WebhookURLReplace, nil
		}
		return WebhookURLPreserve, nil
	}
	switch intent {
	case WebhookURLPreserve, WebhookURLReplace, WebhookURLClear:
		return intent, nil
	default:
		return "", validationError("webhook_url_intent must be preserve, replace, or clear.")
	}
}

func validateStoredWebhookURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || parsed.Host == "" {
		return validationError("The configured webhook URL is invalid.")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return validationError("The configured webhook URL is invalid.")
	}
	return nil
}

func validateReplacementWebhookURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || parsed.Host == "" {
		return validationError("Webhook URL must be a valid HTTPS URL.")
	}
	if parsed.User != nil {
		return validationError("Embedded URL credentials are not supported.")
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if isLocalWebhookHost(parsed.Hostname()) {
			return nil
		}
		return validationError("Use HTTPS for public endpoints. HTTP is allowed only for localhost, private IP addresses, and .local or .internal hosts.")
	default:
		return validationError("Webhook URL must use HTTPS. HTTP is allowed only for supported local or internal endpoints.")
	}
}

func isLocalWebhookHost(host string) bool {
	normalized := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if normalized == "localhost" || strings.HasSuffix(normalized, ".localhost") ||
		strings.HasSuffix(normalized, ".local") || strings.HasSuffix(normalized, ".internal") {
		return true
	}
	ip := net.ParseIP(normalized)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

func settingsResponse(settings Settings, intent WebhookURLIntent) SettingsResponse {
	configured := strings.TrimSpace(settings.WebhookURL) != ""
	return SettingsResponse{
		Enabled:           settings.Enabled,
		WebhookConfigured: configured,
		WebhookURLMasked:  maskWebhookURL(settings.WebhookURL),
		WebhookURLIntent:  intent,
		EventTypes:        append([]string(nil), settings.EventTypes...),
		LastDelivery:      safeDeliveryStatus(settings.LastDelivery),
		SupportedEvents:   SupportedEvents(),
	}
}

func maskWebhookURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || parsed.Hostname() == "" {
		if strings.TrimSpace(raw) == "" {
			return ""
		}
		return "Configured endpoint (masked)"
	}
	host := parsed.Hostname()
	if port := parsed.Port(); port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	masked := (&url.URL{Scheme: parsed.Scheme, Host: host}).String()
	if parsed.EscapedPath() != "" && parsed.EscapedPath() != "/" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		masked += "/••••"
	}
	return masked
}

func validationError(message string) error {
	return &ValidationError{Message: message}
}

func safeWebhookRequestError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errors.New("webhook request failed")
}

func safeDeliveryError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "Webhook delivery was canceled."
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Webhook delivery timed out."
	}
	message := strings.TrimSpace(err.Error())
	if strings.HasPrefix(message, "webhook returned HTTP ") {
		return truncate(message, 240)
	}
	return "Webhook delivery failed."
}

func safeDeliveryStatus(status *DeliveryStatus) *DeliveryStatus {
	if status == nil {
		return nil
	}
	safe := *status
	if strings.TrimSpace(safe.Error) != "" {
		safe.Error = safeDeliveryError(errors.New(safe.Error))
	}
	return &safe
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
