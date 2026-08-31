package service

import (
	"context"
	"encoding/json"
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
		tmpl, err := notificationService.GetTemplate(ctx, NotificationEmailEventUpstreamPriceMonitor, locale)
		require.NoError(t, err)
		variables := notificationService.runtimeVariables(ctx, NotificationEmailEventUpstreamPriceMonitor, locale, NotificationEmailSendInput{
			Variables: upstreamPriceMonitorNotificationVariables(payload, locale),
		})
		preview, err := renderNotificationEmail(
			NotificationEmailEventUpstreamPriceMonitor,
			tmpl.Subject,
			tmpl.HTML,
			variables,
			upstreamPriceMonitorNotificationRawHTMLVariables(payload, locale),
		)
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

func TestUpstreamPriceMonitorNotifierRendersReadablePriceEmail(t *testing.T) {
	ctx := context.Background()
	repo := newNotificationEmailMemorySettingRepo()
	require.NoError(t, repo.Set(ctx, SettingKeySiteName, "LLMRoute"))
	notificationService := NewNotificationEmailService(repo, nil)
	payload := upstreamPriceMonitorTestNotificationPayload(86, UpstreamPriceMonitorNotificationApplied)
	oldCacheWrite, measuredCacheWrite, suggestedCacheWrite := 0.3, 0.4, 0.48
	oldRequest, measuredRequest, suggestedRequest := 0.002, 0.0021, 0.00252
	payload.Models[0].OldPrices.CacheWritePerMillion = &oldCacheWrite
	payload.Models[0].MeasuredPrices.CacheWritePerMillion = &measuredCacheWrite
	payload.Models[0].SuggestedPrices.CacheWritePerMillion = &suggestedCacheWrite
	payload.Models[0].OldPrices.PerRequestLTE256K = &oldRequest
	payload.Models[0].MeasuredPrices.PerRequestLTE256K = &measuredRequest
	payload.Models[0].SuggestedPrices.PerRequestLTE256K = &suggestedRequest

	for _, locale := range []string{"en", "zh-CN"} {
		variables := notificationService.runtimeVariables(ctx, NotificationEmailEventUpstreamPriceMonitor, locale, NotificationEmailSendInput{
			Variables: upstreamPriceMonitorNotificationVariables(payload, locale),
		})
		tmpl, err := notificationService.GetTemplate(ctx, NotificationEmailEventUpstreamPriceMonitor, locale)
		require.NoError(t, err)
		preview, err := renderNotificationEmail(
			NotificationEmailEventUpstreamPriceMonitor,
			tmpl.Subject,
			tmpl.HTML,
			variables,
			upstreamPriceMonitorNotificationRawHTMLVariables(payload, locale),
		)
		require.NoError(t, err)
		require.Equal(t, "[LLMRoute] "+payload.Action.subjectLabel(locale == "zh-CN")+" · MiniMax-M3", preview.Subject)
		require.NotContains(t, preview.HTML, "<pre")
		require.Contains(t, preview.HTML, `<table class="pricing">`)
		require.Contains(t, preview.HTML, `$0.21 / 1M`)
		require.Contains(t, preview.HTML, `$0.252 / 1M`)
		require.Contains(t, preview.HTML, `20.00%`)
		require.NotContains(t, preview.HTML, `class="error-card"`)
		if locale == "zh-CN" {
			require.Contains(t, preview.HTML, "上游实测成本")
			require.Contains(t, preview.HTML, "20%上浮后新售价")
			require.Contains(t, preview.HTML, "展示价格与厂商倍率未修改")
		} else {
			require.Contains(t, preview.HTML, "Measured upstream cost")
			require.Contains(t, preview.HTML, "New price (+20%)")
			require.Contains(t, preview.HTML, "Display prices and provider multipliers were not changed")
		}
		require.NotContains(t, preview.HTML, `×0.12`)
	}
}

func TestUpstreamPriceMonitorNotifierRawHTMLValuesAreEscapedAndErrorIsConditional(t *testing.T) {
	payload := upstreamPriceMonitorTestNotificationPayload(87, UpstreamPriceMonitorNotificationApplyFailed)
	payload.Models[0].Model = `<img src=x onerror="alert(1)">`
	payload.Error = `failed <svg onload="alert(2)"> token=super-secret`

	raw := upstreamPriceMonitorNotificationRawHTMLVariables(payload, "zh")
	require.Contains(t, raw["monitor_price_rows"], `&lt;img src=x onerror=&#34;alert(1)&#34;&gt;`)
	require.NotContains(t, raw["monitor_price_rows"], `<img src=x`)
	require.NotContains(t, raw["monitor_multiplier_cards"], `&lt;img src=x onerror=&#34;alert(1)&#34;&gt;`)
	require.NotContains(t, raw["monitor_multiplier_cards"], `<img src=x`)
	require.Contains(t, raw["monitor_error_card"], `&lt;svg onload=&#34;alert(2)&#34;&gt;`)
	require.NotContains(t, raw["monitor_error_card"], `<svg onload`)
	require.NotContains(t, raw["monitor_error_card"], "super-secret")

	payload.Error = ""
	raw = upstreamPriceMonitorNotificationRawHTMLVariables(payload, "zh")
	require.Empty(t, raw["monitor_error_card"])
}

func TestUpstreamPriceMonitorNotifierExplainsSkippedFixedFeeModel(t *testing.T) {
	payload := upstreamPriceMonitorTestNotificationPayload(98, UpstreamPriceMonitorNotificationApplied)
	fixedFee, tokenPrice := 0.001, 0.2
	payload.Models = append(payload.Models, UpstreamPriceMonitorNotificationModel{
		Model: "fixed-fee-model",
		MeasuredPrices: domain.UpstreamPriceVector{
			FixedPerRequest: &fixedFee, InputPerMillion: &tokenPrice,
		},
	})
	raw := upstreamPriceMonitorNotificationRawHTMLVariables(payload, "zh")
	require.Contains(t, raw["monitor_error_card"], "fixed-fee-model")
	require.Contains(t, raw["monitor_error_card"], "已跳过且未改价")
	require.NotContains(t, raw["monitor_price_rows"], "fixed-fee-model")
	variables := upstreamPriceMonitorNotificationVariables(payload, "zh")
	require.Contains(t, variables["monitor_conclusion"], "1 个模型")
}

func TestUpstreamPriceMonitorOfficialPreviewUsesSafeSampleRows(t *testing.T) {
	svc := NewNotificationEmailService(newNotificationEmailMemorySettingRepo(), nil)
	for _, locale := range []string{"en", "zh"} {
		preview, err := svc.PreviewTemplate(context.Background(), NotificationEmailPreviewInput{
			Event:  NotificationEmailEventUpstreamPriceMonitor,
			Locale: locale,
		})
		require.NoError(t, err)
		require.Contains(t, preview.HTML, `<tr class="price-row">`)
		require.NotContains(t, preview.HTML, `&lt;tr class=&#34;price-row&#34;&gt;`)
		require.NotContains(t, preview.HTML, "{{")
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
			require.Contains(t, preview.Subject, action.subjectLabel(locale == "zh"))
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

func TestUpstreamPriceMonitorNotifierNoChangeAppliedRunIsSilent(t *testing.T) {
	ctx := context.Background()
	repo := newNotificationEmailMemorySettingRepo()
	setUpstreamPriceMonitorNotificationRecipients(t, repo, "ops@example.com")
	sender := &upstreamPriceMonitorNotificationSenderStub{}
	notifier := &UpstreamPriceMonitorEmailNotifier{settingRepo: repo, sender: sender, timeout: time.Second}
	payload := upstreamPriceMonitorTestNotificationPayload(90, UpstreamPriceMonitorNotificationApplied)
	payload.AppliedModels = 0
	payload.Models[0].SuggestedPrices = payload.Models[0].OldPrices

	notifier.send(ctx, payload)
	require.Equal(t, int32(0), sender.calls.Load())
	fingerprint, err := repo.GetValue(ctx, upstreamPriceMonitorNotificationStateSettingKey)
	require.NoError(t, err)
	require.Equal(t, upstreamPriceMonitorHealthyFingerprint, fingerprint)
}

func TestUpstreamPriceMonitorNotifierSendsOnlyFirstUnchangedFailureAcrossRuns(t *testing.T) {
	ctx := context.Background()
	repo := newNotificationEmailMemorySettingRepo()
	setUpstreamPriceMonitorNotificationRecipients(t, repo, "ops@example.com")
	sender := &upstreamPriceMonitorNotificationSenderStub{}
	notifier := &UpstreamPriceMonitorEmailNotifier{settingRepo: repo, sender: sender, timeout: time.Second}

	payload := upstreamPriceMonitorTestNotificationPayload(91, UpstreamPriceMonitorNotificationFailed)
	payload.Error = "upstream usage ledger unavailable"
	notifier.send(ctx, payload)
	payload.RunID = 92
	notifier.send(ctx, payload)
	require.Equal(t, int32(1), sender.calls.Load(), "the same persistent failure must not email every run")

	payload.RunID = 93
	payload.Error = "upstream billing context changed"
	notifier.send(ctx, payload)
	require.Equal(t, int32(2), sender.calls.Load(), "a genuinely different failure state should notify")
}

func TestUpstreamPriceMonitorNotifierHealthyNoChangeResetsFailureFingerprint(t *testing.T) {
	ctx := context.Background()
	repo := newNotificationEmailMemorySettingRepo()
	setUpstreamPriceMonitorNotificationRecipients(t, repo, "ops@example.com")
	sender := &upstreamPriceMonitorNotificationSenderStub{}
	notifier := &UpstreamPriceMonitorEmailNotifier{settingRepo: repo, sender: sender, timeout: time.Second}

	failure := upstreamPriceMonitorTestNotificationPayload(94, UpstreamPriceMonitorNotificationFailed)
	failure.Error = "same persistent failure"
	notifier.send(ctx, failure)
	healthy := upstreamPriceMonitorTestNotificationPayload(95, UpstreamPriceMonitorNotificationApplied)
	healthy.AppliedModels = 0
	healthy.Models[0].SuggestedPrices = healthy.Models[0].OldPrices
	notifier.send(ctx, healthy)
	failure.RunID = 96
	notifier.send(ctx, failure)
	require.Equal(t, int32(2), sender.calls.Load(), "the failure should notify again after a healthy transition")
}

func TestNotifyRunPassesAppliedCountAndExcludesDisplayOrPassiveEvidence(t *testing.T) {
	oldPrice, measuredPrice, suggestedPrice := 0.1, 0.2, 0.24
	repo := &upstreamPriceNotificationRunRepository{
		activeProbeTestRepository: &activeProbeTestRepository{},
		evidence: []domain.UpstreamPriceEvidence{
			{ModelName: "active-token", BillingMode: DisplayBillingModeToken,
				Status: domain.UpstreamPriceEvidenceStatusTrusted, Source: domain.UpstreamPriceEvidenceSourceActiveProbe,
				Prices:          domain.UpstreamPriceVector{InputPerMillion: &measuredPrice},
				CurrentPrices:   domain.UpstreamPriceVector{InputPerMillion: &oldPrice},
				SuggestedPrices: domain.UpstreamPriceVector{InputPerMillion: &suggestedPrice}},
			{ModelName: "display-price-page", BillingMode: DisplayBillingModePerRequest,
				Status: domain.UpstreamPriceEvidenceStatusTrusted, Source: domain.UpstreamPriceEvidenceSourcePricePage,
				Prices: domain.UpstreamPriceVector{PerRequestLTE256K: &measuredPrice}},
			{ModelName: "passive-token", BillingMode: DisplayBillingModeToken,
				Status: domain.UpstreamPriceEvidenceStatusTrusted, Source: domain.UpstreamPriceEvidenceSourceUserRequest,
				Prices: domain.UpstreamPriceVector{InputPerMillion: &measuredPrice}},
		},
	}
	capture := &upstreamPriceNotificationCapture{}
	svc := NewUpstreamPriceMonitorService(repo, nil, nil)
	svc.notifier = capture
	svc.notifyRun(context.Background(), &domain.UpstreamPriceMonitorRun{
		ID: 97, Summary: map[string]any{"applied_models": float64(1)},
	}, UpstreamPriceMonitorNotificationApplied, "")

	require.Len(t, capture.payloads, 1)
	require.Equal(t, 1, capture.payloads[0].AppliedModels)
	require.Len(t, capture.payloads[0].Models, 1)
	require.Equal(t, "active-token", capture.payloads[0].Models[0].Model)
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
		RunID:         runID,
		Action:        action,
		AppliedModels: 1,
		OccurredAt:    time.Date(2026, time.August, 31, 1, 2, 3, 0, time.UTC),
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

type upstreamPriceNotificationRunRepository struct {
	*activeProbeTestRepository
	evidence []domain.UpstreamPriceEvidence
}

func (r *upstreamPriceNotificationRunRepository) ListEvidenceByRun(context.Context, int64) ([]domain.UpstreamPriceEvidence, error) {
	return append([]domain.UpstreamPriceEvidence(nil), r.evidence...), nil
}

type upstreamPriceNotificationCapture struct {
	payloads []UpstreamPriceMonitorNotificationPayload
}

func (c *upstreamPriceNotificationCapture) Notify(_ context.Context, payload UpstreamPriceMonitorNotificationPayload) {
	c.payloads = append(c.payloads, payload)
}

func (s *upstreamPriceMonitorNotificationSenderStub) Send(ctx context.Context, _ NotificationEmailSendInput) error {
	s.calls.Add(1)
	if !s.blockUntilContextDone {
		return nil
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
