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

	DestinationWebhook  = "webhook"
	DestinationDiscord  = "discord"
	DestinationTelegram = "telegram"

	EventUpdateComplete     = "update.complete"
	EventScheduleRunFailed  = "schedule.run.failed"
	EventScheduleRunSkipped = "schedule.run.skipped"
	EventBackupRestore      = "backup.restore"
	EventTest               = "notification.test"

	defaultAttempts                     = 3
	defaultQueueSize                    = 64
	defaultAdmissionPersistenceAttempts = 20
	defaultOutboxPollInterval           = 250 * time.Millisecond
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
	Enabled           bool
	WebhookURL        string
	DiscordEnabled    bool
	DiscordWebhookURL string
	TelegramEnabled   bool
	TelegramBotToken  string
	TelegramChatID    string
	EventTypes        []string
	LastDelivery      *DeliveryStatus
	LastDeliveries    map[string]*DeliveryStatus
}

type SettingsResponse struct {
	Enabled           bool             `json:"enabled"`
	WebhookConfigured bool             `json:"webhook_configured"`
	WebhookURLMasked  string           `json:"webhook_url_masked,omitempty"`
	WebhookURLIntent  WebhookURLIntent `json:"webhook_url_intent"`
	EventTypes        []string         `json:"event_types"`
	SupportedEvents   []string         `json:"supported_events"`
	Discord           DiscordResponse  `json:"discord"`
	Telegram          TelegramResponse `json:"telegram"`
}

type SettingsUpdate struct {
	Enabled          bool             `json:"enabled"`
	WebhookURL       string           `json:"webhook_url,omitempty"`
	WebhookURLIntent WebhookURLIntent `json:"webhook_url_intent,omitempty"`
	EventTypes       []string         `json:"event_types"`
	Discord          *DiscordUpdate   `json:"discord,omitempty"`
	Telegram         *TelegramUpdate  `json:"telegram,omitempty"`
}

type DiscordResponse struct {
	Enabled          bool             `json:"enabled"`
	Configured       bool             `json:"configured"`
	WebhookURLMasked string           `json:"webhook_url_masked,omitempty"`
	WebhookURLIntent WebhookURLIntent `json:"webhook_url_intent"`
}

type DiscordUpdate struct {
	Enabled          bool             `json:"enabled"`
	WebhookURL       string           `json:"webhook_url,omitempty"`
	WebhookURLIntent WebhookURLIntent `json:"webhook_url_intent,omitempty"`
}

type TelegramResponse struct {
	Enabled           bool             `json:"enabled"`
	Configured        bool             `json:"configured"`
	BotTokenMasked    string           `json:"bot_token_masked,omitempty"`
	ChatIDMasked      string           `json:"chat_id_masked,omitempty"`
	CredentialsIntent WebhookURLIntent `json:"credentials_intent"`
}

type TelegramUpdate struct {
	Enabled           bool             `json:"enabled"`
	BotToken          string           `json:"bot_token,omitempty"`
	ChatID            string           `json:"chat_id,omitempty"`
	CredentialsIntent WebhookURLIntent `json:"credentials_intent,omitempty"`
}

type persistedSettings struct {
	Enabled             bool                       `json:"enabled"`
	LegacyWebhookURL    string                     `json:"webhook_url,omitempty"`
	EncryptedWebhookURL string                     `json:"webhook_url_enc,omitempty"`
	DiscordEnabled      bool                       `json:"discord_enabled,omitempty"`
	DiscordWebhookURL   string                     `json:"discord_webhook_url_enc,omitempty"`
	TelegramEnabled     bool                       `json:"telegram_enabled,omitempty"`
	TelegramBotToken    string                     `json:"telegram_bot_token_enc,omitempty"`
	TelegramChatID      string                     `json:"telegram_chat_id_enc,omitempty"`
	EventTypes          []string                   `json:"event_types"`
	LastDelivery        *DeliveryStatus            `json:"last_delivery,omitempty"`
	LastDeliveries      map[string]*DeliveryStatus `json:"last_deliveries,omitempty"`
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

type DeliveryOutcome string

const (
	DeliveryOutcomeSucceeded DeliveryOutcome = "succeeded"
	DeliveryOutcomeRetrying  DeliveryOutcome = "retrying"
	DeliveryOutcomeFailed    DeliveryOutcome = "failed"
)

type DeliveryStatus struct {
	Destination         string          `json:"destination,omitempty"`
	EventType           string          `json:"event_type"`
	Action              string          `json:"action"`
	TargetName          string          `json:"target_name"`
	Outcome             DeliveryOutcome `json:"outcome"`
	Success             bool            `json:"success"`
	Attempts            int             `json:"attempts"`
	StatusCode          int             `json:"status_code,omitempty"`
	Error               string          `json:"error,omitempty"`
	AttemptedAt         string          `json:"attempted_at"`
	CompletedAt         string          `json:"completed_at"`
	DeliveredAt         string          `json:"delivered_at,omitempty"`
	DurationMS          int64           `json:"duration_ms"`
	ConsecutiveFailures int             `json:"consecutive_failures"`
	NextRetryAt         string          `json:"next_retry_at,omitempty"`
}

type DeliveryDiagnostics struct {
	LastAttempt   *DeliveryStatus            `json:"last_attempt,omitempty"`
	LastAttempts  map[string]*DeliveryStatus `json:"last_attempts,omitempty"`
	PendingCount  int                        `json:"pending_count,omitempty"`
	RetryingCount int                        `json:"retrying_count,omitempty"`
}

type DeliveryIntent struct {
	ID         string
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
	DeliveryDiagnostics() (DeliveryDiagnostics, error)
	Accept(DeliveryIntent) Admission
	TestDelivery(context.Context) (DeliveryStatus, error)
	Close(context.Context) error
}

type DestinationTester interface {
	TestDestination(context.Context, string) (DeliveryStatus, error)
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
	PollInterval    time.Duration
	ClaimTTL        time.Duration
	Retention       time.Duration
	NewID           func() string
}

type Service struct {
	deps ServiceDeps

	settingsMu  sync.Mutex
	lifecycleMu sync.Mutex
	wake        chan struct{}
	closing     bool
	cancel      context.CancelFunc
	done        chan struct{}
	initErr     error
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
	if deps.PollInterval <= 0 {
		deps.PollInterval = defaultOutboxPollInterval
	}
	if deps.ClaimTTL <= 0 {
		deps.ClaimTTL = defaultOutboxClaimTTL
	}
	if deps.Retention <= 0 {
		deps.Retention = defaultOutboxRetention
	}
	if deps.NewID == nil {
		deps.NewID = newOutboxID
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		deps:   deps,
		wake:   make(chan struct{}, deps.QueueSize),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	hasOutstanding := false
	if deps.DB != nil {
		db := deps.DB()
		s.initErr = EnsureSchema(db)
		if s.initErr == nil {
			s.initErr = recoverExpiredOutboxClaims(ctx, db, deps.Now(), deps.ClaimTTL)
		}
		if s.initErr == nil {
			_, s.initErr = pruneOutbox(ctx, db, deps.Now(), deps.Retention)
		}
		if s.initErr == nil {
			counts, err := loadOutboxCounts(ctx, db)
			s.initErr = err
			hasOutstanding = counts.Pending > 0 || counts.Retrying > 0
		}
	}
	go s.run(ctx)
	if s.initErr == nil && hasOutstanding {
		s.signalWorker()
	}
	return s
}

func (s *Service) run(ctx context.Context) {
	defer close(s.done)
	var retryTimer *time.Timer
	var retryC <-chan time.Time
	claimFailures := 0
	for {
		select {
		case <-ctx.Done():
			if retryTimer != nil {
				retryTimer.Stop()
			}
			return
		case <-s.wake:
			if retryTimer != nil {
				retryTimer.Stop()
				retryTimer = nil
				retryC = nil
			}
		case <-retryC:
			retryTimer = nil
			retryC = nil
		}
		if err := recoverExpiredOutboxClaims(ctx, s.database(), s.deps.Now(), s.deps.ClaimTTL); err != nil {
			s.deps.Logf("notification outbox claim recovery failed: %v", err)
		}
		claimFailed := false
		for ctx.Err() == nil {
			row, err := claimNextOutboxRow(ctx, s.database(), s.deps.Now())
			if err != nil {
				s.deps.Logf("notification outbox claim failed: %v", err)
				claimFailures++
				claimFailed = true
				break
			}
			claimFailures = 0
			if row == nil {
				if s.isClosing() {
					return
				}
				break
			}
			s.processOutboxRow(ctx, *row)
		}
		if claimFailed && s.isClosing() {
			return
		}
		if claimFailed {
			if claimFailures < defaultAttempts {
				delay := s.deps.PollInterval
				if delay <= 0 {
					delay = defaultOutboxPollInterval
				}
				retryTimer = time.NewTimer(delay)
				retryC = retryTimer.C
			}
			continue
		}
		if ctx.Err() != nil {
			continue
		}
		nextAt, scheduled, err := nextOutboxWake(ctx, s.database(), s.deps.ClaimTTL)
		if err != nil || !scheduled {
			continue
		}
		delay := nextAt.Sub(s.deps.Now().UTC())
		if delay < 0 {
			delay = 0
		}
		retryTimer = time.NewTimer(delay)
		retryC = retryTimer.C
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
	destinations := enabledDestinations(settings)
	if strings.TrimSpace(intent.ID) == "" {
		intent.ID = s.deps.NewID()
	}
	rows, err := buildOutboxRows(intent, eventType, destinations, s.deps.Now())
	if err != nil {
		return Admission{State: AdmissionRejected, Error: err.Error()}
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closing {
		return Admission{State: AdmissionClosing}
	}
	if s.initErr != nil {
		return Admission{State: AdmissionRejected, Error: "notification delivery persistence is unavailable"}
	}
	now := s.deps.Now().UTC().Format(time.RFC3339Nano)
	if err := s.persistOutboxRows(rows, now); err != nil {
		return Admission{State: AdmissionRejected, Error: "notification delivery could not be persisted"}
	}
	s.signalWorker()
	return Admission{State: AdmissionAdmitted}
}

func (s *Service) persistOutboxRows(rows []outboxRow, now string) error {
	var err error
	for attempt := 1; attempt <= defaultAdmissionPersistenceAttempts; attempt++ {
		err = insertOutboxRows(context.Background(), s.database(), rows, now)
		if err == nil || !isSQLiteContention(err) || attempt == defaultAdmissionPersistenceAttempts {
			return err
		}
		delay := s.deps.Backoff(attempt)
		if delay <= 0 || delay > 100*time.Millisecond {
			delay = 5 * time.Millisecond
		}
		time.Sleep(delay)
	}
	return err
}

func isSQLiteContention(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "database is busy") ||
		strings.Contains(message, "sqlite_busy") || strings.Contains(message, "sqlite_locked")
}

func (s *Service) Close(ctx context.Context) error {
	s.lifecycleMu.Lock()
	if !s.closing {
		s.closing = true
		s.signalWorker()
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

func (s *Service) database() *sql.DB {
	if s == nil || s.deps.DB == nil {
		return nil
	}
	return s.deps.DB()
}

func (s *Service) signalWorker() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) isClosing() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return s.closing
}

func (s *Service) processOutboxRow(ctx context.Context, row outboxRow) {
	settings, err := s.currentSettings()
	if err != nil {
		s.releaseClaim(row, err)
		return
	}
	if !destinationEnabled(settings, row.Destination) || !destinationConfigured(settings, row.Destination) {
		now := s.deps.Now().UTC().Format(time.RFC3339Nano)
		persistCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err := s.retrySQLiteContention(func() error {
			return completeOutboxRow(persistCtx, s.database(), row.ID, outboxOutcome{
				State: outboxStateSkipped, Attempts: row.Attempts, CompletedAt: now,
			}, now)
		})
		if err != nil {
			s.deps.Logf("notification outbox skip persistence failed for %q: %v", row.ID, err)
		}
		return
	}

	attemptCtx, cancelAttempt := context.WithTimeout(ctx, s.deps.DeliveryTimeout)
	status, deliveryErr := s.attemptOutboxDestination(attemptCtx, settings, row)
	cancelAttempt()
	outcome := outboxOutcome{
		Attempts:    status.Attempts,
		AttemptedAt: status.AttemptedAt,
		CompletedAt: status.CompletedAt,
		StatusCode:  status.StatusCode,
		Error:       status.Error,
		DurationMS:  status.DurationMS,
	}
	if deliveryErr == nil {
		outcome.State = outboxStateSucceeded
	} else if status.Attempts < defaultAttempts {
		delay := s.deps.Backoff(status.Attempts)
		if delay < 0 {
			delay = 0
		}
		outcome.State = outboxStateRetrying
		outcome.NextAttemptAt = s.deps.Now().UTC().Add(delay).Format(time.RFC3339Nano)
		status.Outcome = DeliveryOutcomeRetrying
		status.NextRetryAt = outcome.NextAttemptAt
	} else {
		outcome.State = outboxStateFailed
		status.Outcome = DeliveryOutcomeFailed
		status.NextRetryAt = ""
	}
	persistCtx, cancelPersist := context.WithTimeout(context.Background(), 2*time.Second)
	err = s.retrySQLiteContention(func() error {
		return completeOutboxRow(persistCtx, s.database(), row.ID, outcome, s.deps.Now().UTC().Format(time.RFC3339Nano))
	})
	cancelPersist()
	if err != nil {
		s.deps.Logf("notification outbox outcome persistence failed for %q: %v", row.ID, err)
		return
	}
	if err := s.retrySQLiteContention(func() error { return s.storeLastDelivery(row.Destination, status) }); err != nil {
		s.deps.Logf("notification diagnostic persistence failed for %q: %v", row.ID, err)
	}
	if outcome.State == outboxStateRetrying {
		s.signalWorker()
	}
}

func (s *Service) attemptOutboxDestination(ctx context.Context, settings Settings, row outboxRow) (DeliveryStatus, error) {
	status := DeliveryStatus{
		Destination: row.Destination,
		EventType:   row.EventType,
		Action:      row.Intent.Action,
		TargetName:  row.Intent.TargetName,
		Attempts:    row.Attempts + 1,
	}
	attemptedAt := s.deps.Now().UTC()
	status.AttemptedAt = attemptedAt.Format(time.RFC3339)
	payload, err := buildPayload(row.EventType, row.Intent)
	if err != nil {
		return failedDeliveryStatus(status, attemptedAt, s.deps.Now().UTC(), err)
	}
	endpoint, body, err := destinationRequest(settings, row.Destination, payload)
	if err != nil {
		return failedDeliveryStatus(status, attemptedAt, s.deps.Now().UTC(), err)
	}
	if previous := safeDeliveryStatus(settings.LastDeliveries[row.Destination]); previous != nil {
		status.ConsecutiveFailures = previous.ConsecutiveFailures
	} else if row.Destination == DestinationWebhook {
		if previous := safeDeliveryStatus(settings.LastDelivery); previous != nil {
			status.ConsecutiveFailures = previous.ConsecutiveFailures
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		err = errors.New("webhook request could not be created")
	} else {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "SimpleLinuxUpdater/notification-hook")
		resp, doErr := s.deps.HTTPClient.Do(req)
		if doErr != nil {
			err = safeWebhookRequestError(doErr)
		} else {
			status.StatusCode = resp.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				err = nil
			} else {
				err = fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
			}
		}
	}
	completedAt := s.deps.Now().UTC()
	status.CompletedAt = completedAt.Format(time.RFC3339)
	status.DeliveredAt = status.CompletedAt
	status.DurationMS = nonNegativeMilliseconds(completedAt.Sub(attemptedAt))
	if err == nil {
		status.Success = true
		status.Outcome = DeliveryOutcomeSucceeded
		status.ConsecutiveFailures = 0
		return status, nil
	}
	status.Success = false
	status.Error = safeDeliveryError(err)
	status.ConsecutiveFailures++
	return status, err
}

func failedDeliveryStatus(status DeliveryStatus, attemptedAt, completedAt time.Time, err error) (DeliveryStatus, error) {
	status.Success = false
	status.Outcome = DeliveryOutcomeFailed
	status.Error = safeDeliveryError(err)
	status.ConsecutiveFailures++
	status.CompletedAt = completedAt.Format(time.RFC3339)
	status.DeliveredAt = status.CompletedAt
	status.DurationMS = nonNegativeMilliseconds(completedAt.Sub(attemptedAt))
	return status, err
}

func (s *Service) currentSettings() (Settings, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	return s.loadSettings()
}

func (s *Service) releaseClaim(row outboxRow, cause error) {
	now := s.deps.Now().UTC()
	nowText := now.Format(time.RFC3339Nano)
	nextAttempt := now.Add(s.deps.PollInterval).Format(time.RFC3339Nano)
	persistCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.retrySQLiteContention(func() error {
		return releaseOutboxClaim(persistCtx, s.database(), row.ID, nextAttempt, nowText)
	}); err != nil {
		s.deps.Logf("notification outbox claim release failed for %q after %v: %v", row.ID, cause, err)
	}
}

func destinationEnabled(settings Settings, destination string) bool {
	switch destination {
	case DestinationWebhook:
		return settings.Enabled
	case DestinationDiscord:
		return settings.DiscordEnabled
	case DestinationTelegram:
		return settings.TelegramEnabled
	default:
		return false
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
	return settingsResponse(settings, WebhookURLPreserve, WebhookURLPreserve, WebhookURLPreserve), nil
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
		Enabled:           update.Enabled,
		WebhookURL:        current.WebhookURL,
		DiscordEnabled:    current.DiscordEnabled,
		DiscordWebhookURL: current.DiscordWebhookURL,
		TelegramEnabled:   current.TelegramEnabled,
		TelegramBotToken:  current.TelegramBotToken,
		TelegramChatID:    current.TelegramChatID,
		EventTypes:        events,
		LastDelivery:      current.LastDelivery,
		LastDeliveries:    cloneDeliveryStatuses(current.LastDeliveries),
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
	discordIntent := WebhookURLPreserve
	if update.Discord != nil {
		discordIntent, err = normalizeSecretIntent(update.Discord.WebhookURLIntent)
		if err != nil {
			return SettingsResponse{}, err
		}
		settings.DiscordEnabled = update.Discord.Enabled
		switch discordIntent {
		case WebhookURLReplace:
			replacement := strings.TrimSpace(update.Discord.WebhookURL)
			if replacement == "" {
				return SettingsResponse{}, validationError("A replacement Discord webhook URL is required.")
			}
			if err := validateDiscordWebhookURL(replacement); err != nil {
				return SettingsResponse{}, err
			}
			settings.DiscordWebhookURL = replacement
		case WebhookURLClear:
			if strings.TrimSpace(update.Discord.WebhookURL) != "" {
				return SettingsResponse{}, validationError("discord.webhook_url must be empty when clearing the configured URL.")
			}
			if update.Discord.Enabled {
				return SettingsResponse{}, validationError("Disable Discord delivery before clearing the configured URL.")
			}
			settings.DiscordWebhookURL = ""
		case WebhookURLPreserve:
			if strings.TrimSpace(update.Discord.WebhookURL) != "" {
				return SettingsResponse{}, validationError("discord.webhook_url must be empty when preserving the configured URL.")
			}
		}
		if settings.DiscordEnabled && settings.DiscordWebhookURL == "" {
			return SettingsResponse{}, validationError("Discord enablement requires a configured webhook URL.")
		}
	}
	telegramIntent := WebhookURLPreserve
	if update.Telegram != nil {
		telegramIntent, err = normalizeSecretIntent(update.Telegram.CredentialsIntent)
		if err != nil {
			return SettingsResponse{}, err
		}
		settings.TelegramEnabled = update.Telegram.Enabled
		switch telegramIntent {
		case WebhookURLReplace:
			token := strings.TrimSpace(update.Telegram.BotToken)
			chatID := strings.TrimSpace(update.Telegram.ChatID)
			if err := validateTelegramCredentials(token, chatID); err != nil {
				return SettingsResponse{}, err
			}
			settings.TelegramBotToken = token
			settings.TelegramChatID = chatID
		case WebhookURLClear:
			if strings.TrimSpace(update.Telegram.BotToken) != "" || strings.TrimSpace(update.Telegram.ChatID) != "" {
				return SettingsResponse{}, validationError("Telegram credentials must be empty when clearing the configured integration.")
			}
			if update.Telegram.Enabled {
				return SettingsResponse{}, validationError("Disable Telegram delivery before clearing the configured integration.")
			}
			settings.TelegramBotToken = ""
			settings.TelegramChatID = ""
		case WebhookURLPreserve:
			if strings.TrimSpace(update.Telegram.BotToken) != "" || strings.TrimSpace(update.Telegram.ChatID) != "" {
				return SettingsResponse{}, validationError("Telegram credentials must be empty when preserving the configured integration.")
			}
		}
		if settings.TelegramEnabled && (settings.TelegramBotToken == "" || settings.TelegramChatID == "") {
			return SettingsResponse{}, validationError("Telegram enablement requires a configured bot token and chat ID.")
		}
	}
	if err := s.saveSettings(settings); err != nil {
		return SettingsResponse{}, err
	}
	return settingsResponse(settings, intent, discordIntent, telegramIntent), nil
}

func (s *Service) DeliveryDiagnostics() (DeliveryDiagnostics, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	var settings Settings
	err := s.retrySQLiteContention(func() error {
		var err error
		settings, err = s.loadSettings()
		return err
	})
	if err != nil {
		return DeliveryDiagnostics{}, err
	}
	var counts outboxCounts
	err = s.retrySQLiteContention(func() error {
		var err error
		counts, err = loadOutboxCounts(context.Background(), s.database())
		return err
	})
	if err != nil {
		return DeliveryDiagnostics{}, err
	}
	return DeliveryDiagnostics{
		LastAttempt:   latestDeliveryStatus(settings),
		LastAttempts:  cloneDeliveryStatuses(settings.LastDeliveries),
		PendingCount:  counts.Pending,
		RetryingCount: counts.Retrying,
	}, nil
}

func (s *Service) retrySQLiteContention(operation func() error) error {
	var err error
	for attempt := 1; attempt <= defaultAttempts; attempt++ {
		err = operation()
		if err == nil || !isSQLiteContention(err) || attempt == defaultAttempts {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	return err
}

func (s *Service) TestDelivery(ctx context.Context) (DeliveryStatus, error) {
	return s.TestDestination(ctx, DestinationWebhook)
}

func (s *Service) TestDestination(ctx context.Context, destination string) (DeliveryStatus, error) {
	evt := DeliveryIntent{
		CreatedAt:  s.deps.Now().UTC().Format(time.RFC3339),
		Actor:      "admin",
		Action:     EventTest,
		TargetType: "notification",
		TargetName: strings.TrimSpace(destination),
		Status:     "test",
		Message:    "Notification test",
		MetaJSON:   `{"source":"admin"}`,
	}
	return s.deliverDestination(ctx, evt, strings.TrimSpace(destination), true)
}

func (s *Service) deliverDestination(ctx context.Context, evt DeliveryIntent, destination string, force bool) (DeliveryStatus, error) {
	settings, eventType, err := s.notificationPlan(evt, force)
	if err != nil {
		return DeliveryStatus{}, err
	}
	if !destinationConfigured(settings, destination) {
		return DeliveryStatus{}, validationError(fmt.Sprintf("%s is not configured.", destinationLabel(destination)))
	}
	return s.deliverToDestination(ctx, settings, evt, eventType, destination)
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
	} else if isSuccessfulNoOpUpdate(evt) {
		return settings, "", nil
	} else if !anyDestinationEnabled(settings) || !eventEnabled(settings.EventTypes, eventType) {
		return settings, "", nil
	}
	return settings, eventType, nil
}

func isSuccessfulNoOpUpdate(evt DeliveryIntent) bool {
	if strings.TrimSpace(evt.Action) != EventUpdateComplete || !strings.EqualFold(strings.TrimSpace(evt.Status), "success") {
		return false
	}
	var meta struct {
		UpgradeCompleted *bool `json:"upgrade_completed"`
	}
	if err := json.Unmarshal([]byte(evt.MetaJSON), &meta); err != nil || meta.UpgradeCompleted == nil {
		return false
	}
	return !*meta.UpgradeCompleted
}

func (s *Service) deliverToDestination(ctx context.Context, settings Settings, evt DeliveryIntent, eventType, destination string) (DeliveryStatus, error) {
	payload, err := buildPayload(eventType, evt)
	if err != nil {
		return DeliveryStatus{}, err
	}
	endpoint, body, err := destinationRequest(settings, destination, payload)
	if err != nil {
		return DeliveryStatus{}, err
	}
	status := DeliveryStatus{
		Destination: destination,
		EventType:   eventType,
		Action:      evt.Action,
		TargetName:  evt.TargetName,
	}
	if previous := safeDeliveryStatus(settings.LastDeliveries[destination]); previous != nil {
		status.ConsecutiveFailures = previous.ConsecutiveFailures
	} else if destination == DestinationWebhook {
		if previous := safeDeliveryStatus(settings.LastDelivery); previous != nil {
			status.ConsecutiveFailures = previous.ConsecutiveFailures
		}
	}
	var lastErr error
	for attempt := 1; attempt <= defaultAttempts; attempt++ {
		attemptedAt := s.deps.Now().UTC()
		status.Attempts = attempt
		status.AttemptedAt = attemptedAt.Format(time.RFC3339)
		status.CompletedAt = ""
		status.DeliveredAt = ""
		status.DurationMS = 0
		status.StatusCode = 0
		status.Error = ""
		status.NextRetryAt = ""
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
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
				completedAt := s.deps.Now().UTC()
				status.Success = true
				status.Outcome = DeliveryOutcomeSucceeded
				status.Error = ""
				status.CompletedAt = completedAt.Format(time.RFC3339)
				status.DeliveredAt = status.CompletedAt
				status.DurationMS = nonNegativeMilliseconds(completedAt.Sub(attemptedAt))
				status.ConsecutiveFailures = 0
				status.NextRetryAt = ""
				if err := s.storeLastDelivery(destination, status); err != nil {
					return status, fmt.Errorf("record notification delivery outcome: %w", err)
				}
				return status, nil
			}
			lastErr = fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
		}
		completedAt := s.deps.Now().UTC()
		status.Success = false
		status.CompletedAt = completedAt.Format(time.RFC3339)
		status.DeliveredAt = status.CompletedAt
		status.DurationMS = nonNegativeMilliseconds(completedAt.Sub(attemptedAt))
		status.Error = safeDeliveryError(lastErr)
		status.ConsecutiveFailures++
		if attempt < defaultAttempts {
			delay := s.deps.Backoff(attempt)
			if delay < 0 {
				delay = 0
			}
			status.Outcome = DeliveryOutcomeRetrying
			status.NextRetryAt = completedAt.Add(delay).Format(time.RFC3339)
			if err := s.storeLastDelivery(destination, status); err != nil {
				return status, fmt.Errorf("record notification retry outcome: %w", err)
			}
			select {
			case <-ctx.Done():
				lastErr = ctx.Err()
				attempt = defaultAttempts
			case <-time.After(delay):
			}
		}
	}
	completedAt := s.deps.Now().UTC()
	status.Success = false
	status.Outcome = DeliveryOutcomeFailed
	status.Error = safeDeliveryError(lastErr)
	if status.AttemptedAt == "" {
		status.AttemptedAt = completedAt.Format(time.RFC3339)
	}
	status.CompletedAt = completedAt.Format(time.RFC3339)
	status.DeliveredAt = status.CompletedAt
	status.NextRetryAt = ""
	if status.ConsecutiveFailures == 0 {
		status.ConsecutiveFailures = 1
	}
	if err := s.storeLastDelivery(destination, status); err != nil {
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
	settings.DiscordEnabled = stored.DiscordEnabled
	settings.TelegramEnabled = stored.TelegramEnabled
	settings.EventTypes = stored.EventTypes
	settings.LastDelivery = safeDeliveryStatus(stored.LastDelivery)
	settings.LastDeliveries = cloneDeliveryStatuses(stored.LastDeliveries)
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
	if strings.TrimSpace(stored.DiscordWebhookURL) != "" {
		settings.DiscordWebhookURL, err = s.decryptStoredSecret(stored.DiscordWebhookURL)
		if err != nil {
			return Settings{}, errors.New("protected Discord webhook URL cannot be loaded")
		}
	}
	if strings.TrimSpace(stored.TelegramBotToken) != "" {
		settings.TelegramBotToken, err = s.decryptStoredSecret(stored.TelegramBotToken)
		if err != nil {
			return Settings{}, errors.New("protected Telegram bot token cannot be loaded")
		}
	}
	if strings.TrimSpace(stored.TelegramChatID) != "" {
		settings.TelegramChatID, err = s.decryptStoredSecret(stored.TelegramChatID)
		if err != nil {
			return Settings{}, errors.New("protected Telegram chat ID cannot be loaded")
		}
	}
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
		Enabled:         settings.Enabled,
		DiscordEnabled:  settings.DiscordEnabled,
		TelegramEnabled: settings.TelegramEnabled,
		EventTypes:      append([]string(nil), settings.EventTypes...),
		LastDelivery:    safeDeliveryStatus(settings.LastDelivery),
		LastDeliveries:  cloneDeliveryStatuses(settings.LastDeliveries),
	}
	var err error
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
	if stored.DiscordWebhookURL, err = s.encryptStoredSecret(settings.DiscordWebhookURL); err != nil {
		return errors.New("discord webhook URL could not be protected")
	}
	if stored.TelegramBotToken, err = s.encryptStoredSecret(settings.TelegramBotToken); err != nil {
		return errors.New("telegram bot token could not be protected")
	}
	if stored.TelegramChatID, err = s.encryptStoredSecret(settings.TelegramChatID); err != nil {
		return errors.New("telegram chat ID could not be protected")
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

func (s *Service) storeLastDelivery(destination string, status DeliveryStatus) error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	settings, err := s.loadSettings()
	if err != nil {
		return err
	}
	if settings.LastDeliveries == nil {
		settings.LastDeliveries = map[string]*DeliveryStatus{}
	}
	settings.LastDeliveries[destination] = safeDeliveryStatus(&status)
	if destination == DestinationWebhook {
		settings.LastDelivery = safeDeliveryStatus(&status)
	}
	return s.saveSettings(settings)
}

func (s *Service) decryptStoredSecret(ciphertext string) (string, error) {
	if s.deps.DecryptSecret == nil {
		return "", errors.New("secret decryption is unavailable")
	}
	value, err := s.deps.DecryptSecret(ciphertext)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func (s *Service) encryptStoredSecret(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if s.deps.EncryptSecret == nil {
		return "", errors.New("secret encryption is unavailable")
	}
	return s.deps.EncryptSecret(value)
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
	rewrap := func(ciphertext string) (string, error) {
		if strings.TrimSpace(ciphertext) == "" {
			return "", nil
		}
		if decrypt == nil || encrypt == nil {
			return "", errors.New("notification integration secret protection is unavailable")
		}
		plaintext, err := decrypt(ciphertext)
		if err != nil {
			return "", errors.New("decrypt restored notification integration secret")
		}
		protected, err := encrypt(plaintext)
		if err != nil {
			return "", errors.New("encrypt restored notification integration secret")
		}
		return protected, nil
	}
	if stored.DiscordWebhookURL, err = rewrap(stored.DiscordWebhookURL); err != nil {
		return err
	}
	if stored.TelegramBotToken, err = rewrap(stored.TelegramBotToken); err != nil {
		return err
	}
	if stored.TelegramChatID, err = rewrap(stored.TelegramChatID); err != nil {
		return err
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

func settingsResponse(settings Settings, intent, discordIntent, telegramIntent WebhookURLIntent) SettingsResponse {
	configured := strings.TrimSpace(settings.WebhookURL) != ""
	return SettingsResponse{
		Enabled:           settings.Enabled,
		WebhookConfigured: configured,
		WebhookURLMasked:  maskWebhookURL(settings.WebhookURL),
		WebhookURLIntent:  intent,
		EventTypes:        append([]string(nil), settings.EventTypes...),
		SupportedEvents:   SupportedEvents(),
		Discord: DiscordResponse{
			Enabled:          settings.DiscordEnabled,
			Configured:       strings.TrimSpace(settings.DiscordWebhookURL) != "",
			WebhookURLMasked: maskWebhookURL(settings.DiscordWebhookURL),
			WebhookURLIntent: discordIntent,
		},
		Telegram: TelegramResponse{
			Enabled:           settings.TelegramEnabled,
			Configured:        strings.TrimSpace(settings.TelegramBotToken) != "" && strings.TrimSpace(settings.TelegramChatID) != "",
			BotTokenMasked:    maskTelegramToken(settings.TelegramBotToken),
			ChatIDMasked:      maskTelegramChatID(settings.TelegramChatID),
			CredentialsIntent: telegramIntent,
		},
	}
}

func nonNegativeMilliseconds(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return duration.Milliseconds()
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
	switch safe.Outcome {
	case DeliveryOutcomeSucceeded, DeliveryOutcomeRetrying, DeliveryOutcomeFailed:
	default:
		if safe.Success {
			safe.Outcome = DeliveryOutcomeSucceeded
		} else {
			safe.Outcome = DeliveryOutcomeFailed
		}
	}
	safe.Success = safe.Outcome == DeliveryOutcomeSucceeded
	if safe.AttemptedAt == "" {
		safe.AttemptedAt = safe.CompletedAt
	}
	if safe.AttemptedAt == "" {
		safe.AttemptedAt = safe.DeliveredAt
	}
	if safe.CompletedAt == "" {
		safe.CompletedAt = safe.DeliveredAt
	}
	if safe.DeliveredAt == "" {
		safe.DeliveredAt = safe.CompletedAt
	}
	if safe.DurationMS < 0 {
		safe.DurationMS = 0
	}
	if safe.Outcome == DeliveryOutcomeSucceeded {
		safe.ConsecutiveFailures = 0
		safe.NextRetryAt = ""
	} else if safe.ConsecutiveFailures <= 0 {
		safe.ConsecutiveFailures = 1
	}
	if safe.Outcome != DeliveryOutcomeRetrying {
		safe.NextRetryAt = ""
	}
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
