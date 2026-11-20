package notifications

import (
	"strings"
	"testing"
)

func TestResumptionReviewSurvivesSavingTheSameDestination(t *testing.T) {
	h := newConditionHarness(t)
	review := func() string {
		t.Helper()
		queues, revision, err := h.service.RestoreDeliveryResumption(t.Context(), h.now)
		if err != nil || len(queues) != 1 || !queues[0].Configured {
			t.Fatalf("review = %+v, %v", queues, err)
		}
		return revision
	}
	before := review()
	h.service.deps.EncryptSecret = func(value string) (string, error) {
		return "sealed:" + value, nil
	}
	h.service.deps.DecryptSecret = func(value string) (string, error) {
		return strings.TrimPrefix(value, "sealed:"), nil
	}
	update := SettingsUpdate{Enabled: true, EventTypes: SupportedEvents(), ReminderIntervalMinutes: 5}
	if _, err := h.service.SaveSettings(update); err != nil {
		t.Fatal(err)
	}
	if after := review(); before != after {
		t.Fatal("saving the same notification destination invalidated the review")
	}
	update.WebhookURLIntent, update.WebhookURL = WebhookURLReplace, "https://new.example.test/webhook"
	if _, err := h.service.SaveSettings(update); err != nil {
		t.Fatal(err)
	}
	if after := review(); before == after {
		t.Fatal("rotating the notification destination preserved the previous review")
	}
}
