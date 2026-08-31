package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
)

const upstreamPriceMonitorNotificationTimeout = 8 * time.Second

type UpstreamPriceMonitorNotificationAction string

const (
	UpstreamPriceMonitorNotificationSuggested   UpstreamPriceMonitorNotificationAction = "suggested"
	UpstreamPriceMonitorNotificationPartial     UpstreamPriceMonitorNotificationAction = "partial"
	UpstreamPriceMonitorNotificationFailed      UpstreamPriceMonitorNotificationAction = "failed"
	UpstreamPriceMonitorNotificationApplied     UpstreamPriceMonitorNotificationAction = "applied"
	UpstreamPriceMonitorNotificationApplyFailed UpstreamPriceMonitorNotificationAction = "apply_failed"
	UpstreamPriceMonitorNotificationRolledBack  UpstreamPriceMonitorNotificationAction = "rolled_back"
)

var (
	upstreamPriceMonitorSecretPattern = regexp.MustCompile(`(?i)\b(authorization|api[ _-]?key|token|secret|password|credential)\b\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;]+)`)
	upstreamPriceMonitorBearerPattern = regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]+`)
	upstreamPriceMonitorSKPattern     = regexp.MustCompile(`(?i)\bsk-[a-z0-9_-]{8,}\b`)
	upstreamPriceMonitorOpaquePattern = regexp.MustCompile(`\b(?:[a-fA-F0-9]{32,}|[A-Za-z0-9_-]{48,})\b`)
	upstreamPriceMonitorURLPattern    = regexp.MustCompile(`(?i)https?://[^\s]+`)
	upstreamPriceMonitorRawPattern    = regexp.MustCompile(`(?i)\b(request|response|body|payload)\b\s*[:=].*$`)
)

// UpstreamPriceMonitorNotifier is intentionally fire-and-forget. Notification
// delivery must never become part of the monitor's pricing transaction.
type UpstreamPriceMonitorNotifier interface {
	Notify(context.Context, UpstreamPriceMonitorNotificationPayload)
}

type UpstreamPriceMonitorNotificationModel struct {
	Model                      string
	OldPrices                  domain.UpstreamPriceVector
	MeasuredPrices             domain.UpstreamPriceVector
	SuggestedPrices            domain.UpstreamPriceVector
	DisplayMultiplierCurrent   *float64
	DisplayMultiplierSuggested *float64
}

type UpstreamPriceMonitorNotificationPayload struct {
	RunID      int64
	Action     UpstreamPriceMonitorNotificationAction
	Models     []UpstreamPriceMonitorNotificationModel
	OccurredAt time.Time
	Error      string
}

type upstreamPriceMonitorNotificationSender interface {
	Send(context.Context, NotificationEmailSendInput) error
	ResolveRecipientLocale(context.Context, int64, string) string
}

// UpstreamPriceMonitorEmailNotifier reuses the operations alert recipients and
// NotificationEmailService's durable delivery key. A delivery is deduplicated
// by run ID, action, and recipient.
type UpstreamPriceMonitorEmailNotifier struct {
	settingRepo SettingRepository
	sender      upstreamPriceMonitorNotificationSender
	timeout     time.Duration
}

func NewUpstreamPriceMonitorEmailNotifier(
	settingRepo SettingRepository,
	notificationEmailService *NotificationEmailService,
) *UpstreamPriceMonitorEmailNotifier {
	return &UpstreamPriceMonitorEmailNotifier{
		settingRepo: settingRepo,
		sender:      notificationEmailService,
		timeout:     upstreamPriceMonitorNotificationTimeout,
	}
}

func (n *UpstreamPriceMonitorEmailNotifier) Notify(ctx context.Context, payload UpstreamPriceMonitorNotificationPayload) {
	if n == nil || n.settingRepo == nil || n.sender == nil || payload.RunID <= 0 || !payload.Action.valid() {
		return
	}
	payload = cloneUpstreamPriceMonitorNotificationPayload(payload)
	baseCtx := context.Background()
	if ctx != nil {
		baseCtx = context.WithoutCancel(ctx)
	}
	timeout := n.timeout
	if timeout <= 0 {
		timeout = upstreamPriceMonitorNotificationTimeout
	}
	go func() {
		notifyCtx, cancel := context.WithTimeout(baseCtx, timeout)
		defer cancel()
		n.send(notifyCtx, payload)
	}()
}

func (n *UpstreamPriceMonitorEmailNotifier) send(ctx context.Context, payload UpstreamPriceMonitorNotificationPayload) {
	recipients, err := n.recipients(ctx)
	if err != nil {
		slog.Warn("upstream price monitor notification recipients unavailable", "run_id", payload.RunID, "action", payload.Action, "err", err)
		return
	}
	if len(recipients) == 0 {
		return
	}

	for _, recipient := range recipients {
		if err := ctx.Err(); err != nil {
			slog.Warn("upstream price monitor notification timed out", "run_id", payload.RunID, "action", payload.Action)
			return
		}
		locale := n.sender.ResolveRecipientLocale(ctx, 0, recipient)
		if err := n.sender.Send(ctx, NotificationEmailSendInput{
			Event:          NotificationEmailEventUpstreamPriceMonitor,
			Locale:         locale,
			RecipientEmail: recipient,
			RecipientName:  emailRecipientName(recipient),
			SourceType:     "upstream_price_monitor",
			SourceID:       strconv.FormatInt(payload.RunID, 10),
			ReminderKey:    string(payload.Action),
			Variables:      upstreamPriceMonitorNotificationVariables(payload, locale),
		}); err != nil {
			slog.Warn(
				"upstream price monitor notification delivery failed",
				"run_id", payload.RunID,
				"action", payload.Action,
				"recipient_hash", notificationEmailHash(recipient),
				"err", err,
			)
		}
	}
}

func (n *UpstreamPriceMonitorEmailNotifier) recipients(ctx context.Context) ([]string, error) {
	raw, err := n.settingRepo.GetValue(ctx, SettingKeyOpsEmailNotificationConfig)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var cfg OpsEmailNotificationConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("decode operations notification config: %w", err)
	}
	if !cfg.Alert.Enabled {
		return nil, nil
	}
	return normalizeEmails(cfg.Alert.Recipients), nil
}

func (action UpstreamPriceMonitorNotificationAction) valid() bool {
	switch action {
	case UpstreamPriceMonitorNotificationSuggested,
		UpstreamPriceMonitorNotificationPartial,
		UpstreamPriceMonitorNotificationFailed,
		UpstreamPriceMonitorNotificationApplied,
		UpstreamPriceMonitorNotificationApplyFailed,
		UpstreamPriceMonitorNotificationRolledBack:
		return true
	default:
		return false
	}
}

func upstreamPriceMonitorNotificationVariables(payload UpstreamPriceMonitorNotificationPayload, locale string) map[string]string {
	zh := normalizeNotificationLocale(locale) == notificationEmailLocaleChinese
	models := append([]UpstreamPriceMonitorNotificationModel(nil), payload.Models...)
	sort.SliceStable(models, func(i, j int) bool {
		return strings.ToLower(strings.TrimSpace(models[i].Model)) < strings.ToLower(strings.TrimSpace(models[j].Model))
	})

	modelNames := make([]string, 0, len(models))
	oldPrices := make([]string, 0, len(models))
	measuredPrices := make([]string, 0, len(models))
	suggestedPrices := make([]string, 0, len(models))
	displayMultipliers := make([]string, 0, len(models))
	for _, model := range models {
		name := strings.TrimSpace(model.Model)
		if name == "" {
			name = "-"
		}
		modelNames = append(modelNames, name)
		oldPrices = append(oldPrices, name+": "+formatUpstreamPriceMonitorVector(model.OldPrices, zh))
		measuredPrices = append(measuredPrices, name+": "+formatUpstreamPriceMonitorVector(model.MeasuredPrices, zh))
		suggestedPrices = append(suggestedPrices, name+": "+formatUpstreamPriceMonitorVector(model.SuggestedPrices, zh))
		displayMultipliers = append(displayMultipliers, name+": "+formatUpstreamPriceMonitorMultiplier(model.DisplayMultiplierCurrent, model.DisplayMultiplierSuggested))
	}

	occurredAt := payload.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	return map[string]string{
		"monitor_action":             payload.Action.label(zh),
		"monitor_run_id":             strconv.FormatInt(payload.RunID, 10),
		"monitor_models":             upstreamPriceMonitorLinesOrDash(modelNames),
		"monitor_old_prices":         upstreamPriceMonitorLinesOrDash(oldPrices),
		"monitor_measured_prices":    upstreamPriceMonitorLinesOrDash(measuredPrices),
		"monitor_suggested_prices":   upstreamPriceMonitorLinesOrDash(suggestedPrices),
		"monitor_display_multiplier": upstreamPriceMonitorLinesOrDash(displayMultipliers),
		"monitor_occurred_at":        occurredAt.UTC().Format(time.RFC3339),
		"monitor_error":              sanitizeUpstreamPriceMonitorNotificationError(payload.Error),
	}
}

func (action UpstreamPriceMonitorNotificationAction) label(zh bool) string {
	if zh {
		switch action {
		case UpstreamPriceMonitorNotificationSuggested:
			return "建议调价"
		case UpstreamPriceMonitorNotificationPartial:
			return "部分完成"
		case UpstreamPriceMonitorNotificationFailed:
			return "失败"
		case UpstreamPriceMonitorNotificationApplied:
			return "已应用"
		case UpstreamPriceMonitorNotificationApplyFailed:
			return "应用失败"
		case UpstreamPriceMonitorNotificationRolledBack:
			return "已回滚"
		}
	}
	switch action {
	case UpstreamPriceMonitorNotificationSuggested:
		return "suggested"
	case UpstreamPriceMonitorNotificationPartial:
		return "partial"
	case UpstreamPriceMonitorNotificationFailed:
		return "failed"
	case UpstreamPriceMonitorNotificationApplied:
		return "applied"
	case UpstreamPriceMonitorNotificationApplyFailed:
		return "apply failed"
	case UpstreamPriceMonitorNotificationRolledBack:
		return "rolled back"
	default:
		return "unknown"
	}
}

func formatUpstreamPriceMonitorVector(vector domain.UpstreamPriceVector, zh bool) string {
	type dimension struct {
		zhLabel string
		enLabel string
		value   *float64
		unit    string
	}
	dimensions := []dimension{
		{zhLabel: "输入", enLabel: "input", value: vector.InputPerMillion, unit: "/1M"},
		{zhLabel: "输出", enLabel: "output", value: vector.OutputPerMillion, unit: "/1M"},
		{zhLabel: "缓存写入", enLabel: "cache write", value: vector.CacheWritePerMillion, unit: "/1M"},
		{zhLabel: "缓存读取", enLabel: "cache read", value: vector.CacheReadPerMillion, unit: "/1M"},
		{zhLabel: "按次 ≤256K", enLabel: "per request ≤256K", value: vector.PerRequestLTE256K, unit: "/request"},
		{zhLabel: "按次 256K-512K", enLabel: "per request 256K-512K", value: vector.PerRequest256K512K, unit: "/request"},
		{zhLabel: "按次 >512K", enLabel: "per request >512K", value: vector.PerRequestGT512K, unit: "/request"},
	}
	parts := make([]string, 0, len(dimensions))
	for _, item := range dimensions {
		if item.value == nil {
			continue
		}
		label := item.enLabel
		unit := item.unit
		if zh {
			label = item.zhLabel
			if unit == "/request" {
				unit = "/次"
			}
		}
		parts = append(parts, fmt.Sprintf("%s $%s%s", label, formatUpstreamPriceMonitorNumber(*item.value), unit))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "; ")
}

func formatUpstreamPriceMonitorMultiplier(current, suggested *float64) string {
	if current == nil && suggested == nil {
		return "-"
	}
	if current == nil {
		return "- → " + formatUpstreamPriceMonitorNumber(*suggested)
	}
	if suggested == nil {
		return formatUpstreamPriceMonitorNumber(*current) + " → -"
	}
	return formatUpstreamPriceMonitorNumber(*current) + " → " + formatUpstreamPriceMonitorNumber(*suggested)
}

func formatUpstreamPriceMonitorNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func upstreamPriceMonitorLinesOrDash(lines []string) string {
	if len(lines) == 0 {
		return "-"
	}
	return strings.Join(lines, "\n")
}

func sanitizeUpstreamPriceMonitorNotificationError(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "-"
	}
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	value = upstreamPriceMonitorBearerPattern.ReplaceAllString(value, "Bearer [redacted]")
	value = upstreamPriceMonitorRawPattern.ReplaceAllString(value, "$1=[redacted]")
	value = upstreamPriceMonitorSecretPattern.ReplaceAllString(value, "$1=[redacted]")
	value = upstreamPriceMonitorSKPattern.ReplaceAllString(value, "[redacted]")
	value = upstreamPriceMonitorURLPattern.ReplaceAllString(value, "[url redacted]")
	value = upstreamPriceMonitorOpaquePattern.ReplaceAllString(value, "[redacted]")
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 240 {
		value = string(runes[:240]) + "…"
	}
	if strings.TrimSpace(value) == "" {
		return "[redacted]"
	}
	return value
}

func cloneUpstreamPriceMonitorNotificationPayload(payload UpstreamPriceMonitorNotificationPayload) UpstreamPriceMonitorNotificationPayload {
	cloned := payload
	cloned.Models = make([]UpstreamPriceMonitorNotificationModel, len(payload.Models))
	for i, model := range payload.Models {
		cloned.Models[i] = model
		cloned.Models[i].OldPrices = cloneUpstreamPriceVector(model.OldPrices)
		cloned.Models[i].MeasuredPrices = cloneUpstreamPriceVector(model.MeasuredPrices)
		cloned.Models[i].SuggestedPrices = cloneUpstreamPriceVector(model.SuggestedPrices)
		cloned.Models[i].DisplayMultiplierCurrent = cloneUpstreamPriceFloat(model.DisplayMultiplierCurrent)
		cloned.Models[i].DisplayMultiplierSuggested = cloneUpstreamPriceFloat(model.DisplayMultiplierSuggested)
	}
	return cloned
}

func cloneUpstreamPriceVector(vector domain.UpstreamPriceVector) domain.UpstreamPriceVector {
	return domain.UpstreamPriceVector{
		InputPerMillion:      cloneUpstreamPriceFloat(vector.InputPerMillion),
		OutputPerMillion:     cloneUpstreamPriceFloat(vector.OutputPerMillion),
		CacheWritePerMillion: cloneUpstreamPriceFloat(vector.CacheWritePerMillion),
		CacheReadPerMillion:  cloneUpstreamPriceFloat(vector.CacheReadPerMillion),
		PerRequestLTE256K:    cloneUpstreamPriceFloat(vector.PerRequestLTE256K),
		PerRequest256K512K:   cloneUpstreamPriceFloat(vector.PerRequest256K512K),
		PerRequestGT512K:     cloneUpstreamPriceFloat(vector.PerRequestGT512K),
	}
}

func cloneUpstreamPriceFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
