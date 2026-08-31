package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestUpstreamPriceMonitorNotifierDeduplicatesRunActionRecipient(t *testing.T) {
	ctx := context.Background()
	repo := newNotificationEmailMemorySettingRepo()
	smtpServer := startNotificationEmailTestSMTPServer(t)
	require.NoError(t, repo.SetMultiple(ctx, smtpServer.settings()))
	setUpstreamPriceMonitorNotificationRecipients(t, repo, "ops@example.com", "OPS@example.com")

	emailService := NewEmailService(repo, nil)
	notificationService := NewNotificationEmailService(repo, emailService)
	notifier := NewUpstreamPriceMonitorEmailNotifier(repo, notificationService)
	payload := upstreamPriceMonitorTestNotificationPayload(81, UpstreamPriceMonitorNotificationSuggested)

	notifier.send(ctx, payload)
	notifier.send(ctx, payload)
	require.Equal(t, int64(1), smtpServer.messageCount())

	payload.Action = UpstreamPriceMonitorNotificationApplied
	notifier.send(ctx, payload)
	require.Equal(t, int64(2), smtpServer.messageCount(), "a different action for the same run is a distinct notification")
}

func TestUpstreamPriceMonitorNotifierTemplateEscapesAndRedacts(t *testing.T) {
	ctx := context.Background()
	repo := newNotificationEmailMemorySettingRepo()
	notificationService := NewNotificationEmailService(repo, nil)
	payload := upstreamPriceMonitorTestNotificationPayload(82, UpstreamPriceMonitorNotificationApplyFailed)
	payload.Models[0].Model = `<script>alert("model")</script>`
	payload.Error = "upstream failed token=super-secret sk-1234567890abcdef hash " + strings.Repeat("a", 64) + " https://example.com/path?key=secret"

	for _, locale := range []string{"en", "zh-CN"} {
		preview, err := notificationService.PreviewTemplate(ctx, NotificationEmailPreviewInput{
			Event:     NotificationEmailEventUpstreamPriceMonitor,
			Locale:    locale,
			Variables: upstreamPriceMonitorNotificationVariables(payload, locale),
		})
		require.NoError(t, err)
		require.NotContains(t, preview.HTML, `<script>alert("model")</script>`)
		require.Contains(t, preview.HTML, `&lt;script&gt;alert`)
		require.NotContains(t, preview.HTML, "super-secret")
		require.NotContains(t, preview.HTML, "sk-1234567890abcdef")
		require.NotContains(t, preview.HTML, strings.Repeat("a", 64))
		require.NotContains(t, preview.HTML, "example.com/path")
		require.Contains(t, preview.HTML, "[redacted]")
	}
}

func TestUpstreamPriceMonitorNotifierSupportsAllActionsAndLocales(t *testing.T) {
	ctx := context.Background()
	notificationService := NewNotificationEmailService(newNotificationEmailMemorySettingRepo(), nil)
	actions := []UpstreamPriceMonitorNotificationAction{
		UpstreamPriceMonitorNotificationSuggested,
		UpstreamPriceMonitorNotificationPartial,
		UpstreamPriceMonitorNotificationFailed,
		UpstreamPriceMonitorNotificationApplied,
		UpstreamPriceMonitorNotificationApplyFailed,
		UpstreamPriceMonitorNotificationRolledBack,
	}
	for _, action := range actions {
		require.True(t, action.valid())
		for _, locale := range []string{"en", "zh"} {
			payload := upstreamPriceMonitorTestNotificationPayload(83, action)
			preview, err := notificationService.PreviewTemplate(ctx, NotificationEmailPreviewInput{
				Event:     NotificationEmailEventUpstreamPriceMonitor,
				Locale:    locale,
				Variables: upstreamPriceMonitorNotificationVariables(payload, locale),
			})
			require.NoError(t, err)
			require.Contains(t, preview.Subject, action.label(locale == "zh"))
			require.NotContains(t, preview.HTML, "{{")
		}
	}
}

func TestUpstreamPriceMonitorNotifierNoRecipientsIsNoop(t *testing.T) {
	ctx := context.Background()
	repo := newNotificationEmailMemorySettingRepo()
	setUpstreamPriceMonitorNotificationRecipients(t, repo)
	sender := &upstreamPriceMonitorNotificationSenderStub{}
	notifier := &UpstreamPriceMonitorEmailNotifier{settingRepo: repo, sender: sender, timeout: time.Second}

	notifier.send(ctx, upstreamPriceMonitorTestNotificationPayload(84, UpstreamPriceMonitorNotificationFailed))
	require.Equal(t, int32(0), sender.calls.Load())
}

func TestUpstreamPriceMonitorNotifierSMTPFailureDoesNotBlockAndUsesTimeout(t *testing.T) {
	repo := newNotificationEmailMemorySettingRepo()
	setUpstreamPriceMonitorNotificationRecipients(t, repo, "ops@example.com")
	sender := &upstreamPriceMonitorNotificationSenderStub{
		blockUntilContextDone: true,
		started:               make(chan struct{}),
		done:                  make(chan struct{}),
	}
	notifier := &UpstreamPriceMonitorEmailNotifier{settingRepo: repo, sender: sender, timeout: 40 * time.Millisecond}

	startedAt := time.Now()
	notifier.Notify(context.Background(), upstreamPriceMonitorTestNotificationPayload(85, UpstreamPriceMonitorNotificationFailed))
	require.Less(t, time.Since(startedAt), 20*time.Millisecond, "Notify must return without waiting for SMTP")

	select {
	case <-sender.started:
	case <-time.After(time.Second):
		t.Fatal("notification send did not start")
	}
	select {
	case <-sender.done:
	case <-time.After(time.Second):
		t.Fatal("notification send did not stop at its timeout")
	}
	require.Equal(t, int32(1), sender.calls.Load())
}

func TestSanitizeUpstreamPriceMonitorNotificationErrorTruncatesAndRedacts(t *testing.T) {
	raw := "failed Authorization: Bearer top-secret token=abc123 password=hunter2 response={\"secret\":\"value\"} " + strings.Repeat("x", 400)
	got := sanitizeUpstreamPriceMonitorNotificationError(raw)
	require.LessOrEqual(t, len([]rune(got)), 241)
	require.NotContains(t, strings.ToLower(got), "top-secret")
	require.NotContains(t, strings.ToLower(got), "abc123")
	require.NotContains(t, strings.ToLower(got), "hunter2")
	require.NotContains(t, strings.ToLower(got), "\"secret\"")
	require.Contains(t, got, "[redacted]")
}

func setUpstreamPriceMonitorNotificationRecipients(t *testing.T, repo SettingRepository, recipients ...string) {
	t.Helper()
	cfg := OpsEmailNotificationConfig{Alert: OpsEmailAlertConfig{Enabled: true, Recipients: recipients}}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, repo.Set(context.Background(), SettingKeyOpsEmailNotificationConfig, string(raw)))
}

func upstreamPriceMonitorTestNotificationPayload(runID int64, action UpstreamPriceMonitorNotificationAction) UpstreamPriceMonitorNotificationPayload {
	oldInput := 0.21
	oldOutput := 0.84
	measuredInput := 0.21
	measuredOutput := 0.84
	suggestedInput := 0.252
	suggestedOutput := 1.008
	currentMultiplier := 0.1
	suggestedMultiplier := 0.12
	return UpstreamPriceMonitorNotificationPayload{
		RunID:      runID,
		Action:     action,
		OccurredAt: time.Date(2026, time.August, 31, 1, 2, 3, 0, time.UTC),
		Models: []UpstreamPriceMonitorNotificationModel{{
			Model: "MiniMax-M3",
			OldPrices: domain.UpstreamPriceVector{
				InputPerMillion:  &oldInput,
				OutputPerMillion: &oldOutput,
			},
			MeasuredPrices: domain.UpstreamPriceVector{
				InputPerMillion:  &measuredInput,
				OutputPerMillion: &measuredOutput,
			},
			SuggestedPrices: domain.UpstreamPriceVector{
				InputPerMillion:  &suggestedInput,
				OutputPerMillion: &suggestedOutput,
			},
			DisplayMultiplierCurrent:   &currentMultiplier,
			DisplayMultiplierSuggested: &suggestedMultiplier,
		}},
	}
}

type upstreamPriceMonitorNotificationSenderStub struct {
	calls                 atomic.Int32
	blockUntilContextDone bool
	started               chan struct{}
	done                  chan struct{}
	startOnce             sync.Once
	doneOnce              sync.Once
}

func (s *upstreamPriceMonitorNotificationSenderStub) Send(ctx context.Context, _ NotificationEmailSendInput) error {
	s.calls.Add(1)
	if !s.blockUntilContextDone {
		return errors.New("smtp failed")
	}
	s.startOnce.Do(func() { close(s.started) })
	<-ctx.Done()
	s.doneOnce.Do(func() { close(s.done) })
	return ctx.Err()
}

func (s *upstreamPriceMonitorNotificationSenderStub) ResolveRecipientLocale(context.Context, int64, string) string {
	return notificationEmailLocaleChinese
}

var _ UpstreamPriceMonitorNotifier = (*UpstreamPriceMonitorEmailNotifier)(nil)
var _ upstreamPriceMonitorNotificationSender = (*upstreamPriceMonitorNotificationSenderStub)(nil)
