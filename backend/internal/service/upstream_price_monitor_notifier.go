package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
)

const (
	upstreamPriceMonitorNotificationTimeout         = 8 * time.Second
	upstreamPriceMonitorNotificationStateSettingKey = "upstream_price_monitor_notification_state"
	upstreamPriceMonitorHealthyFingerprint          = "healthy"
)

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
	RunID         int64
	Action        UpstreamPriceMonitorNotificationAction
	Models        []UpstreamPriceMonitorNotificationModel
	AppliedModels int
	OccurredAt    time.Time
	Error         string
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
	// stateMu serializes the settings-backed fingerprint transition inside one
	// process. SettingRepository has no compare-and-swap primitive, so separate
	// application replicas may still race; the durable email delivery key keeps
	// a duplicate for the same run/action/recipient from being delivered.
	stateMu sync.Mutex
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

	// Keep the read/compare/write transition and delivery together in this
	// process. Repeated unhealthy runs with the same fingerprint stay quiet;
	// a healthy no-change run advances the state without sending an email.
	n.stateMu.Lock()
	defer n.stateMu.Unlock()
	deliver, nextFingerprint, err := n.notificationTransition(ctx, payload)
	if err != nil {
		slog.Warn("upstream price monitor notification state unavailable", "run_id", payload.RunID, "action", payload.Action, "err", err)
		return
	}
	if !deliver {
		if nextFingerprint != "" {
			n.persistNotificationFingerprint(ctx, payload, nextFingerprint)
		}
		return
	}

	delivered := false
	for _, recipient := range recipients {
		if err := ctx.Err(); err != nil {
			slog.Warn("upstream price monitor notification timed out", "run_id", payload.RunID, "action", payload.Action)
			return
		}
		locale := n.sender.ResolveRecipientLocale(ctx, 0, recipient)
		variables := upstreamPriceMonitorNotificationVariables(payload, locale)
		if err := n.sender.Send(ctx, NotificationEmailSendInput{
			Event:          NotificationEmailEventUpstreamPriceMonitor,
			Locale:         locale,
			RecipientEmail: recipient,
			RecipientName:  emailRecipientName(recipient),
			SourceType:     "upstream_price_monitor",
			SourceID:       strconv.FormatInt(payload.RunID, 10),
			ReminderKey:    string(payload.Action),
			Variables:      variables,
			RawHTMLVariables: upstreamPriceMonitorNotificationRawHTMLVariables(
				payload,
				locale,
			),
		}); err != nil {
			slog.Warn(
				"upstream price monitor notification delivery failed",
				"run_id", payload.RunID,
				"action", payload.Action,
				"recipient_hash", notificationEmailHash(recipient),
				"err", err,
			)
			continue
		}
		delivered = true
	}
	if delivered && nextFingerprint != "" {
		n.persistNotificationFingerprint(ctx, payload, nextFingerprint)
	}
}

func (n *UpstreamPriceMonitorEmailNotifier) notificationTransition(
	ctx context.Context,
	payload UpstreamPriceMonitorNotificationPayload,
) (deliver bool, nextFingerprint string, err error) {
	switch payload.Action {
	case UpstreamPriceMonitorNotificationApplied, UpstreamPriceMonitorNotificationRolledBack:
		if payload.AppliedModels > 0 {
			return true, upstreamPriceMonitorHealthyFingerprint, nil
		}
		return false, upstreamPriceMonitorHealthyFingerprint, nil
	case UpstreamPriceMonitorNotificationSuggested:
		if !upstreamPriceMonitorHasTokenPriceChange(payload.Models) {
			return false, upstreamPriceMonitorHealthyFingerprint, nil
		}
	case UpstreamPriceMonitorNotificationPartial,
		UpstreamPriceMonitorNotificationFailed,
		UpstreamPriceMonitorNotificationApplyFailed:
		// These actions represent an unhealthy monitor state. The fingerprint
		// below makes only the first occurrence of an unchanged state noisy.
	default:
		return false, "", nil
	}

	fingerprint := upstreamPriceMonitorNotificationFingerprint(payload)
	current, getErr := n.settingRepo.GetValue(ctx, upstreamPriceMonitorNotificationStateSettingKey)
	if getErr != nil && !errors.Is(getErr, ErrSettingNotFound) {
		return false, "", getErr
	}
	if strings.TrimSpace(current) == fingerprint {
		return false, "", nil
	}
	return true, fingerprint, nil
}

func (n *UpstreamPriceMonitorEmailNotifier) persistNotificationFingerprint(
	ctx context.Context,
	payload UpstreamPriceMonitorNotificationPayload,
	fingerprint string,
) {
	current, err := n.settingRepo.GetValue(ctx, upstreamPriceMonitorNotificationStateSettingKey)
	if err == nil && strings.TrimSpace(current) == fingerprint {
		return
	}
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		slog.Warn("upstream price monitor notification state read failed", "run_id", payload.RunID, "action", payload.Action, "err", err)
		return
	}
	if err := n.settingRepo.Set(ctx, upstreamPriceMonitorNotificationStateSettingKey, fingerprint); err != nil {
		slog.Warn("upstream price monitor notification state write failed", "run_id", payload.RunID, "action", payload.Action, "err", err)
	}
}

func upstreamPriceMonitorHasTokenPriceChange(models []UpstreamPriceMonitorNotificationModel) bool {
	for _, model := range models {
		if upstreamPriceMonitorModelHasUnrepresentableFixedFee(model) {
			continue
		}
		oldValues := []*float64{
			model.OldPrices.InputPerMillion,
			model.OldPrices.OutputPerMillion,
			model.OldPrices.CacheWritePerMillion,
			model.OldPrices.CacheReadPerMillion,
		}
		nextValues := []*float64{
			model.SuggestedPrices.InputPerMillion,
			model.SuggestedPrices.OutputPerMillion,
			model.SuggestedPrices.CacheWritePerMillion,
			model.SuggestedPrices.CacheReadPerMillion,
		}
		for index := range oldValues {
			if nextValues[index] != nil && !sameUpstreamPriceMonitorNotificationFloat(oldValues[index], nextValues[index]) {
				return true
			}
		}
	}
	return false
}

func sameUpstreamPriceMonitorNotificationFloat(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	tolerance := math.Max(1e-12, math.Max(math.Abs(*a), math.Abs(*b))*1e-9)
	return math.Abs(*a-*b) <= tolerance
}

func upstreamPriceMonitorNotificationFingerprint(payload UpstreamPriceMonitorNotificationPayload) string {
	type fingerprintModel struct {
		Model     string                     `json:"model"`
		Old       domain.UpstreamPriceVector `json:"old"`
		Measured  domain.UpstreamPriceVector `json:"measured"`
		Suggested domain.UpstreamPriceVector `json:"suggested"`
	}
	models := sortedUpstreamPriceMonitorNotificationModels(payload.Models)
	stableModels := make([]fingerprintModel, 0, len(models))
	for _, model := range models {
		stableModels = append(stableModels, fingerprintModel{
			Model: strings.ToLower(strings.TrimSpace(model.Model)),
			Old:   tokenOnlyUpstreamPriceVector(model.OldPrices), Measured: tokenOnlyUpstreamPriceVector(model.MeasuredPrices),
			Suggested: tokenOnlyUpstreamPriceVector(model.SuggestedPrices),
		})
	}
	stable := struct {
		Action UpstreamPriceMonitorNotificationAction `json:"action"`
		Models []fingerprintModel                     `json:"models"`
		Error  string                                 `json:"error"`
	}{payload.Action, stableModels, sanitizeUpstreamPriceMonitorNotificationError(payload.Error)}
	raw, _ := json.Marshal(stable)
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum[:])
}

func tokenOnlyUpstreamPriceVector(value domain.UpstreamPriceVector) domain.UpstreamPriceVector {
	return domain.UpstreamPriceVector{
		InputPerMillion:      value.InputPerMillion,
		OutputPerMillion:     value.OutputPerMillion,
		CacheWritePerMillion: value.CacheWritePerMillion,
		CacheReadPerMillion:  value.CacheReadPerMillion,
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
	models := sortedUpstreamPriceMonitorNotificationModels(payload.Models)

	modelNames := make([]string, 0, len(models))
	oldPrices := make([]string, 0, len(models))
	measuredPrices := make([]string, 0, len(models))
	suggestedPrices := make([]string, 0, len(models))
	for _, model := range models {
		name := strings.TrimSpace(model.Model)
		if name == "" {
			name = "-"
		}
		modelNames = append(modelNames, name)
		oldPrices = append(oldPrices, name+": "+formatUpstreamPriceMonitorVector(model.OldPrices, zh))
		measuredPrices = append(measuredPrices, name+": "+formatUpstreamPriceMonitorVector(model.MeasuredPrices, zh))
		suggestedPrices = append(suggestedPrices, name+": "+formatUpstreamPriceMonitorVector(model.SuggestedPrices, zh))
	}
	displayScope := "Display prices and provider multipliers were not changed."
	if zh {
		displayScope = "展示价格与厂商倍率未修改。"
	}

	occurredAt := payload.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	conclusionCount := len(models)
	if payload.Action == UpstreamPriceMonitorNotificationApplied || payload.Action == UpstreamPriceMonitorNotificationRolledBack {
		conclusionCount = payload.AppliedModels
	}
	return map[string]string{
		"monitor_action":             payload.Action.label(zh),
		"monitor_subject_action":     payload.Action.subjectLabel(zh),
		"monitor_subject_models":     upstreamPriceMonitorSubjectModels(models, zh),
		"monitor_conclusion":         payload.Action.conclusion(conclusionCount, zh),
		"monitor_run_id":             strconv.FormatInt(payload.RunID, 10),
		"monitor_model_count":        strconv.Itoa(len(models)),
		"monitor_models":             upstreamPriceMonitorLinesOrDash(modelNames),
		"monitor_old_prices":         upstreamPriceMonitorLinesOrDash(oldPrices),
		"monitor_measured_prices":    upstreamPriceMonitorLinesOrDash(measuredPrices),
		"monitor_suggested_prices":   upstreamPriceMonitorLinesOrDash(suggestedPrices),
		"monitor_display_multiplier": displayScope,
		"monitor_occurred_at":        occurredAt.UTC().Format(time.RFC3339),
		"monitor_error":              sanitizeUpstreamPriceMonitorNotificationError(payload.Error),
	}
}

func upstreamPriceMonitorNotificationRawHTMLVariables(payload UpstreamPriceMonitorNotificationPayload, locale string) map[string]string {
	zh := normalizeNotificationLocale(locale) == notificationEmailLocaleChinese
	models := sortedUpstreamPriceMonitorNotificationModels(payload.Models)
	errorText := strings.TrimSpace(payload.Error)
	if warning := upstreamPriceMonitorFixedFeeWarning(models, zh); warning != "" {
		if errorText != "" {
			errorText += "; "
		}
		errorText += warning
	}
	return map[string]string{
		"monitor_price_rows":       upstreamPriceMonitorPriceRows(models, zh),
		"monitor_multiplier_cards": upstreamPriceMonitorMultiplierCards(nil, zh),
		"monitor_error_card":       upstreamPriceMonitorErrorCard(errorText, zh),
	}
}

func upstreamPriceMonitorModelHasUnrepresentableFixedFee(model UpstreamPriceMonitorNotificationModel) bool {
	for _, value := range []*float64{model.MeasuredPrices.FixedPerRequest, model.SuggestedPrices.FixedPerRequest} {
		if value != nil && math.Abs(*value) > 1e-9 {
			return true
		}
	}
	return false
}

func upstreamPriceMonitorFixedFeeWarning(models []UpstreamPriceMonitorNotificationModel, zh bool) string {
	names := make([]string, 0)
	for _, model := range models {
		if upstreamPriceMonitorModelHasUnrepresentableFixedFee(model) {
			names = append(names, strings.TrimSpace(model.Model))
		}
	}
	if len(names) == 0 {
		return ""
	}
	if zh {
		return "以下模型含渠道 token 定价无法表示的固定请求费，本轮已跳过且未改价：" + strings.Join(names, "、")
	}
	return "Skipped without changing channel prices because fixed request fees cannot be represented: " + strings.Join(names, ", ")
}

func sortedUpstreamPriceMonitorNotificationModels(models []UpstreamPriceMonitorNotificationModel) []UpstreamPriceMonitorNotificationModel {
	sorted := append([]UpstreamPriceMonitorNotificationModel(nil), models...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return strings.ToLower(strings.TrimSpace(sorted[i].Model)) < strings.ToLower(strings.TrimSpace(sorted[j].Model))
	})
	return sorted
}

func upstreamPriceMonitorSubjectModels(models []UpstreamPriceMonitorNotificationModel, zh bool) string {
	if len(models) == 0 {
		if zh {
			return "无模型"
		}
		return "No models"
	}
	name := strings.TrimSpace(models[0].Model)
	if name == "" {
		name = "-"
	}
	if len(models) == 1 {
		return name
	}
	if zh {
		return fmt.Sprintf("%s 等 %d 个模型", name, len(models))
	}
	return fmt.Sprintf("%s +%d", name, len(models)-1)
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

func (action UpstreamPriceMonitorNotificationAction) subjectLabel(zh bool) string {
	if zh {
		switch action {
		case UpstreamPriceMonitorNotificationSuggested:
			return "发现调价建议"
		case UpstreamPriceMonitorNotificationPartial:
			return "价格监控部分完成"
		case UpstreamPriceMonitorNotificationFailed:
			return "价格监控失败"
		case UpstreamPriceMonitorNotificationApplied:
			return "自动改价成功"
		case UpstreamPriceMonitorNotificationApplyFailed:
			return "自动改价失败"
		case UpstreamPriceMonitorNotificationRolledBack:
			return "价格已回滚"
		}
	}
	switch action {
	case UpstreamPriceMonitorNotificationSuggested:
		return "Pricing suggestion"
	case UpstreamPriceMonitorNotificationPartial:
		return "Price monitor partially completed"
	case UpstreamPriceMonitorNotificationFailed:
		return "Price monitor failed"
	case UpstreamPriceMonitorNotificationApplied:
		return "Automatic pricing succeeded"
	case UpstreamPriceMonitorNotificationApplyFailed:
		return "Automatic pricing failed"
	case UpstreamPriceMonitorNotificationRolledBack:
		return "Pricing rolled back"
	default:
		return "Price monitor update"
	}
}

func (action UpstreamPriceMonitorNotificationAction) conclusion(modelCount int, zh bool) string {
	if zh {
		switch action {
		case UpstreamPriceMonitorNotificationSuggested:
			return fmt.Sprintf("检测到 %d 个模型的价格变化，下方为待确认的调价建议。", modelCount)
		case UpstreamPriceMonitorNotificationPartial:
			return fmt.Sprintf("本轮已处理 %d 个模型，但有部分结果需要人工检查。", modelCount)
		case UpstreamPriceMonitorNotificationFailed:
			return "本轮上游价格监控失败，未自动修改价格。"
		case UpstreamPriceMonitorNotificationApplied:
			return fmt.Sprintf("已按上游实测成本上浮 20%%，自动更新 %d 个模型的渠道售价。", modelCount)
		case UpstreamPriceMonitorNotificationApplyFailed:
			return "已生成调价方案，但自动应用失败，请查看错误详情。"
		case UpstreamPriceMonitorNotificationRolledBack:
			return fmt.Sprintf("已回滚 %d 个模型的渠道售价；展示价格与厂商倍率未修改。", modelCount)
		}
	}
	switch action {
	case UpstreamPriceMonitorNotificationSuggested:
		return fmt.Sprintf("Price changes were detected for %d model(s). Review the proposal below.", modelCount)
	case UpstreamPriceMonitorNotificationPartial:
		return fmt.Sprintf("This run processed %d model(s), but some results require review.", modelCount)
	case UpstreamPriceMonitorNotificationFailed:
		return "This upstream pricing run failed. No automatic price changes were made."
	case UpstreamPriceMonitorNotificationApplied:
		return fmt.Sprintf("Channel prices for %d model(s) were updated to measured upstream cost plus 20%%.", modelCount)
	case UpstreamPriceMonitorNotificationApplyFailed:
		return "A pricing proposal was produced, but automatic application failed. Review the error below."
	case UpstreamPriceMonitorNotificationRolledBack:
		return fmt.Sprintf("Channel prices for %d model(s) were rolled back; display prices and provider multipliers were not changed.", modelCount)
	default:
		return "Upstream pricing monitor update."
	}
}

type upstreamPriceMonitorDimension struct {
	zhLabel   string
	enLabel   string
	unit      string
	old       *float64
	measured  *float64
	suggested *float64
}

func upstreamPriceMonitorModelDimensions(model UpstreamPriceMonitorNotificationModel) []upstreamPriceMonitorDimension {
	return []upstreamPriceMonitorDimension{
		{zhLabel: "输入", enLabel: "Input", unit: "/ 1M", old: model.OldPrices.InputPerMillion, measured: model.MeasuredPrices.InputPerMillion, suggested: model.SuggestedPrices.InputPerMillion},
		{zhLabel: "输出", enLabel: "Output", unit: "/ 1M", old: model.OldPrices.OutputPerMillion, measured: model.MeasuredPrices.OutputPerMillion, suggested: model.SuggestedPrices.OutputPerMillion},
		{zhLabel: "缓存写入", enLabel: "Cache write", unit: "/ 1M", old: model.OldPrices.CacheWritePerMillion, measured: model.MeasuredPrices.CacheWritePerMillion, suggested: model.SuggestedPrices.CacheWritePerMillion},
		{zhLabel: "缓存读取", enLabel: "Cache read", unit: "/ 1M", old: model.OldPrices.CacheReadPerMillion, measured: model.MeasuredPrices.CacheReadPerMillion, suggested: model.SuggestedPrices.CacheReadPerMillion},
	}
}

func upstreamPriceMonitorPriceRows(models []UpstreamPriceMonitorNotificationModel, zh bool) string {
	var builder strings.Builder
	for _, model := range models {
		if upstreamPriceMonitorModelHasUnrepresentableFixedFee(model) {
			continue
		}
		name := strings.TrimSpace(model.Model)
		if name == "" {
			name = "-"
		}
		for _, dimension := range upstreamPriceMonitorModelDimensions(model) {
			if dimension.old == nil && dimension.measured == nil && dimension.suggested == nil {
				continue
			}
			label := dimension.enLabel
			unit := dimension.unit
			if zh {
				label = dimension.zhLabel
				if unit == "/ request" {
					unit = "/ 次"
				}
			}
			modelLabel, dimensionLabel, oldLabel, measuredLabel, suggestedLabel, deltaLabel := "Model", "Dimension", "Old channel price", "Measured cost", "New price (+20%)", "Change"
			if zh {
				modelLabel, dimensionLabel, oldLabel, measuredLabel, suggestedLabel, deltaLabel = "模型", "维度", "旧渠道售价", "上游实测成本", "20%上浮后新售价", "变化幅度"
			}
			_, _ = fmt.Fprintf(&builder, `<tr class="price-row"><td data-label="%s"><strong>%s</strong></td><td data-label="%s">%s</td><td data-label="%s">%s</td><td data-label="%s">%s</td><td data-label="%s"><strong class="new-price">%s</strong></td><td data-label="%s"><span class="delta">%s</span></td></tr>`,
				html.EscapeString(modelLabel), html.EscapeString(name),
				html.EscapeString(dimensionLabel), html.EscapeString(label),
				html.EscapeString(oldLabel), html.EscapeString(formatUpstreamPriceMonitorCellPrice(dimension.old, unit)),
				html.EscapeString(measuredLabel), html.EscapeString(formatUpstreamPriceMonitorCellPrice(dimension.measured, unit)),
				html.EscapeString(suggestedLabel), html.EscapeString(formatUpstreamPriceMonitorCellPrice(dimension.suggested, unit)),
				html.EscapeString(deltaLabel), html.EscapeString(formatUpstreamPriceMonitorDelta(dimension.old, dimension.suggested, zh)),
			)
		}
	}
	if builder.Len() > 0 {
		return builder.String()
	}
	message := "No comparable price dimensions were returned."
	if zh {
		message = "本轮未返回可对比的价格维度。"
	}
	return `<tr><td colspan="6" class="empty">` + html.EscapeString(message) + `</td></tr>`
}

func formatUpstreamPriceMonitorCellPrice(value *float64, unit string) string {
	if value == nil {
		return "-"
	}
	return "$" + formatUpstreamPriceMonitorNumber(*value) + " " + unit
}

func formatUpstreamPriceMonitorDelta(old, suggested *float64, zh bool) string {
	if old == nil || suggested == nil {
		return "-"
	}
	if *old == 0 {
		if *suggested == 0 {
			return "0%"
		}
		if zh {
			return "新增"
		}
		return "New"
	}
	delta := (*suggested - *old) / math.Abs(*old) * 100
	return strconv.FormatFloat(delta, 'f', 2, 64) + "%"
}

func upstreamPriceMonitorMultiplierCards(models []UpstreamPriceMonitorNotificationModel, zh bool) string {
	var builder strings.Builder
	for _, model := range models {
		if model.DisplayMultiplierCurrent == nil && model.DisplayMultiplierSuggested == nil {
			continue
		}
		name := strings.TrimSpace(model.Model)
		if name == "" {
			name = "-"
		}
		oldValue := upstreamPriceMonitorMultiplierValue(model.DisplayMultiplierCurrent)
		newValue := upstreamPriceMonitorMultiplierValue(model.DisplayMultiplierSuggested)
		_, _ = fmt.Fprintf(&builder, `<div class="multiplier-card"><div class="multiplier-model">%s</div><div class="multiplier-values"><span>%s</span><span class="arrow">→</span><strong>%s</strong></div></div>`,
			html.EscapeString(name), html.EscapeString(oldValue), html.EscapeString(newValue))
	}
	if builder.Len() > 0 {
		return builder.String()
	}
	message := "Display prices and provider multipliers were not changed."
	if zh {
		message = "展示价格与厂商倍率未修改。"
	}
	return `<div class="empty multiplier-empty">` + html.EscapeString(message) + `</div>`
}

func upstreamPriceMonitorMultiplierValue(value *float64) string {
	if value == nil {
		return "-"
	}
	return "×" + formatUpstreamPriceMonitorNumber(*value)
}

func upstreamPriceMonitorErrorCard(raw string, zh bool) string {
	value := sanitizeUpstreamPriceMonitorNotificationError(raw)
	if value == "-" {
		return ""
	}
	title := "Error details"
	if zh {
		title = "错误详情"
	}
	return `<div class="error-card"><div class="error-title">` + html.EscapeString(title) + `</div><div class="error-message">` + html.EscapeString(value) + `</div></div>`
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
		FixedPerRequest:      cloneUpstreamPriceFloat(vector.FixedPerRequest),
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
