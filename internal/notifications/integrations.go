package notifications

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	telegramBotTokenPattern = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]{20,}$`)
	telegramChatIDPattern   = regexp.MustCompile(`^-?[0-9]+$`)
	telegramUsernamePattern = regexp.MustCompile(`^@[A-Za-z][A-Za-z0-9_]{4,31}$`)
)

type discordWebhookBody struct {
	Username        string                 `json:"username"`
	Embeds          []discordEmbed         `json:"embeds"`
	AllowedMentions discordAllowedMentions `json:"allowed_mentions"`
}

type discordEmbed struct {
	Title       string              `json:"title"`
	Description string              `json:"description"`
	Color       int                 `json:"color"`
	Timestamp   string              `json:"timestamp,omitempty"`
	Fields      []discordEmbedField `json:"fields"`
	Footer      discordEmbedFooter  `json:"footer"`
}

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type discordEmbedFooter struct {
	Text string `json:"text"`
}

type discordAllowedMentions struct {
	Parse []string `json:"parse"`
}

type telegramMessageBody struct {
	ChatID string `json:"chat_id"`
	Text   string `json:"text"`
}

func normalizeSecretIntent(intent WebhookURLIntent) (WebhookURLIntent, error) {
	normalized := WebhookURLIntent(strings.ToLower(strings.TrimSpace(string(intent))))
	if normalized == "" {
		return WebhookURLPreserve, nil
	}
	switch normalized {
	case WebhookURLPreserve, WebhookURLReplace, WebhookURLClear:
		return normalized, nil
	default:
		return "", validationError("integration intent must be preserve, replace, or clear.")
	}
}

func validateDiscordWebhookURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return validationError("Discord webhook URL must be a valid HTTPS URL without embedded credentials.")
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "discord.com" && !strings.HasSuffix(host, ".discord.com") &&
		host != "discordapp.com" && !strings.HasSuffix(host, ".discordapp.com") {
		return validationError("Use an official Discord webhook URL.")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	webhookAt := -1
	for index, part := range parts {
		if part == "webhooks" {
			webhookAt = index
			break
		}
	}
	if webhookAt < 0 || len(parts) < webhookAt+3 || strings.TrimSpace(parts[webhookAt+1]) == "" || strings.TrimSpace(parts[webhookAt+2]) == "" {
		return validationError("Discord webhook URL must include its webhook ID and token.")
	}
	return nil
}

func validateTelegramCredentials(token, chatID string) error {
	if !telegramBotTokenPattern.MatchString(strings.TrimSpace(token)) {
		return validationError("Telegram bot token is invalid.")
	}
	chatID = strings.TrimSpace(chatID)
	if !telegramChatIDPattern.MatchString(chatID) && !telegramUsernamePattern.MatchString(chatID) {
		return validationError("Telegram chat ID must be numeric or a public @channel username.")
	}
	return nil
}

func anyDestinationEnabled(settings Settings) bool {
	return settings.Enabled || settings.DiscordEnabled || settings.TelegramEnabled
}

func enabledDestinations(settings Settings) []string {
	destinations := make([]string, 0, 3)
	if settings.Enabled {
		destinations = append(destinations, DestinationWebhook)
	}
	if settings.DiscordEnabled {
		destinations = append(destinations, DestinationDiscord)
	}
	if settings.TelegramEnabled {
		destinations = append(destinations, DestinationTelegram)
	}
	return destinations
}

func destinationConfigured(settings Settings, destination string) bool {
	switch destination {
	case DestinationWebhook:
		return strings.TrimSpace(settings.WebhookURL) != ""
	case DestinationDiscord:
		return strings.TrimSpace(settings.DiscordWebhookURL) != ""
	case DestinationTelegram:
		return strings.TrimSpace(settings.TelegramBotToken) != "" && strings.TrimSpace(settings.TelegramChatID) != ""
	default:
		return false
	}
}

func destinationLabel(destination string) string {
	switch destination {
	case DestinationWebhook:
		return "Webhook"
	case DestinationDiscord:
		return "Discord"
	case DestinationTelegram:
		return "Telegram"
	default:
		return "Notification destination"
	}
}

func destinationRequest(settings Settings, destination string, payload WebhookPayload) (string, []byte, error) {
	var endpoint string
	var body any
	switch destination {
	case DestinationWebhook:
		if err := validateStoredWebhookURL(settings.WebhookURL); err != nil {
			return "", nil, err
		}
		endpoint = settings.WebhookURL
		body = payload
	case DestinationDiscord:
		if err := validateDiscordWebhookURL(settings.DiscordWebhookURL); err != nil {
			return "", nil, err
		}
		parsed, _ := url.Parse(settings.DiscordWebhookURL)
		query := parsed.Query()
		query.Set("wait", "true")
		parsed.RawQuery = query.Encode()
		endpoint = parsed.String()
		body = buildDiscordBody(payload)
	case DestinationTelegram:
		if err := validateTelegramCredentials(settings.TelegramBotToken, settings.TelegramChatID); err != nil {
			return "", nil, err
		}
		endpoint = fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", settings.TelegramBotToken)
		body = telegramMessageBody{
			ChatID: settings.TelegramChatID,
			Text:   truncate(formatNotificationMessage(payload), 4096),
		}
	default:
		return "", nil, validationError("Unsupported notification destination.")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", nil, fmt.Errorf("notification payload could not be encoded")
	}
	return endpoint, encoded, nil
}

func buildDiscordBody(payload WebhookPayload) discordWebhookBody {
	return discordWebhookBody{
		Username: "SimpleLinuxUpdater",
		Embeds: []discordEmbed{{
			Title:       truncate(notificationTitle(payload), 256),
			Description: truncate(payload.Message, 4096),
			Color:       notificationColor(payload.Status),
			Timestamp:   payload.CreatedAt,
			Fields: []discordEmbedField{
				{Name: "Status", Value: truncate(nonEmpty(payload.Status, "unknown"), 1024), Inline: true},
				{Name: "Target", Value: truncate(nonEmpty(payload.TargetName, payload.TargetType), 1024), Inline: true},
				{Name: "Event", Value: truncate(payload.EventType, 1024), Inline: false},
			},
			Footer: discordEmbedFooter{Text: "SimpleLinuxUpdater"},
		}},
		AllowedMentions: discordAllowedMentions{Parse: []string{}},
	}
}

func formatNotificationMessage(payload WebhookPayload) string {
	return fmt.Sprintf(
		"%s\nStatus: %s\nTarget: %s\nEvent: %s\n%s",
		notificationTitle(payload),
		nonEmpty(payload.Status, "unknown"),
		nonEmpty(payload.TargetName, payload.TargetType),
		payload.EventType,
		payload.Message,
	)
}

func notificationTitle(payload WebhookPayload) string {
	if payload.EventType == EventTest {
		return "SimpleLinuxUpdater notification test"
	}
	return fmt.Sprintf("SimpleLinuxUpdater · %s", payload.EventType)
}

func notificationColor(status string) int {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "success", "succeeded", "complete", "completed":
		return 0x2F9E72
	case "failure", "failed", "error":
		return 0xD95D5D
	case "skipped", "ignored":
		return 0xD29A38
	default:
		return 0x5B8DEF
	}
}

func nonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "-"
}

func maskTelegramToken(raw string) string {
	parts := strings.SplitN(strings.TrimSpace(raw), ":", 2)
	if len(parts) != 2 || parts[0] == "" {
		return ""
	}
	return parts[0] + ":••••"
}

func maskTelegramChatID(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "@") {
		return "@" + truncate(strings.TrimPrefix(value, "@"), 3) + "••••"
	}
	if len(value) <= 4 {
		return "••••"
	}
	return "••••" + value[len(value)-4:]
}

func cloneDeliveryStatuses(values map[string]*DeliveryStatus) map[string]*DeliveryStatus {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]*DeliveryStatus, len(values))
	for destination, status := range values {
		out[destination] = safeDeliveryStatus(status)
	}
	return out
}

func latestDeliveryStatus(settings Settings) *DeliveryStatus {
	latest := safeDeliveryStatus(settings.LastDelivery)
	for _, candidate := range settings.LastDeliveries {
		candidate = safeDeliveryStatus(candidate)
		if candidate == nil {
			continue
		}
		if latest == nil || deliveryStatusTimestamp(candidate) > deliveryStatusTimestamp(latest) {
			latest = candidate
		}
	}
	return latest
}

func deliveryStatusTimestamp(status *DeliveryStatus) string {
	if status == nil {
		return ""
	}
	if strings.TrimSpace(status.AttemptedAt) != "" {
		return status.AttemptedAt
	}
	return status.CompletedAt
}
