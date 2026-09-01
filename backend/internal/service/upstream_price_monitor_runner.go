package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
)

// UpstreamPriceMonitorRunner owns only scheduling. The durable single-running
// constraint and stale-run recovery in the repository are the cross-instance
// backstop, while the service run mutex prevents overlap in one process.
type UpstreamPriceMonitorRunner struct {
	monitor *UpstreamPriceMonitorService
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	once    sync.Once

	perRequestMu      sync.Mutex
	perRequestNextRun time.Time
	tokenMu           sync.Mutex
	tokenNextRun      time.Time
	now               func() time.Time
}

var (
	upstreamPricePageSyncInterval  = 15 * time.Minute
	upstreamPricePageRetryInterval = 5 * time.Minute
	upstreamPricePageSyncTimeout   = time.Minute
)

func NewUpstreamPriceMonitorRunner(monitor *UpstreamPriceMonitorService) *UpstreamPriceMonitorRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &UpstreamPriceMonitorRunner{monitor: monitor, ctx: ctx, cancel: cancel, now: time.Now}
}

func (r *UpstreamPriceMonitorRunner) Start() {
	if r == nil || r.monitor == nil {
		return
	}
	r.once.Do(func() {
		// Public-page pricing has a separate loop so a paid probe run (which can
		// legitimately take tens of minutes) never delays the free 15-minute
		// authoritative page poll.
		r.wg.Add(2)
		go r.loop()
		go r.pageSyncLoop()
	})
}

func (r *UpstreamPriceMonitorRunner) Stop() {
	if r == nil {
		return
	}
	r.cancel()
	r.wg.Wait()
}

func (r *UpstreamPriceMonitorRunner) loop() {
	defer r.wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	r.runIfDue()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.runIfDue()
		}
	}
}

func (r *UpstreamPriceMonitorRunner) pageSyncLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	r.runPageSyncsIfDue()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.runPageSyncsIfDue()
		}
	}
}

func (r *UpstreamPriceMonitorRunner) runIfDue() {
	// A rotating audit sample can require up to seven settled ledger requests
	// per selected model. The service has a tighter 45-minute deadline; this
	// outer allowance leaves time to persist the terminal run state.
	ctx, cancel := context.WithTimeout(r.ctx, 50*time.Minute)
	defer cancel()
	cfg, err := r.monitor.GetConfig(ctx)
	if err != nil || !cfg.Enabled {
		return
	}
	runtime, err := r.monitor.GetRuntime(ctx)
	if err != nil {
		slog.Warn("upstream_price_monitor_runtime_failed", "error", err)
		return
	}
	if runtime.NextRunAt != nil && runtime.NextRunAt.After(time.Now()) {
		return
	}
	_, err = r.monitor.RunOnce(ctx, UpstreamPriceRunOptions{
		Trigger: domain.UpstreamPriceMonitorRunTriggerScheduled,
		// Paid probes are audit-only in every pricing-page control mode.
		DryRun: true,
	})
	if err != nil && !errors.Is(err, ErrUpstreamPriceMonitorRunConflict) {
		slog.Warn("upstream_price_monitor_run_failed", "error", err)
	}
}

func (r *UpstreamPriceMonitorRunner) runPageSyncsIfDue() {
	if r == nil || r.monitor == nil {
		return
	}
	gateCtx, cancel := context.WithTimeout(r.ctx, upstreamPricePageSyncTimeout)
	cfg, err := r.monitor.GetConfig(gateCtx)
	cancel()
	if err != nil {
		slog.Warn("upstream_price_page_sync_config_failed", "error", err)
		return
	}
	// Observe and review are read/review control modes. Only auto_apply grants
	// the scheduler write authority; review uses the explicit administrator
	// sync action, which calls the same atomic service method directly.
	if cfg == nil || !cfg.Enabled || cfg.Mode != domain.UpstreamPriceMonitorModeAutoApply {
		return
	}
	// Each path owns its due time. A failure in one billing mode therefore does
	// not slow the other mode's successful 15-minute polling schedule.
	r.runTokenIfDue()
	r.runPerRequestIfDue()
}

func (r *UpstreamPriceMonitorRunner) runTokenIfDue() {
	if r == nil || r.monitor == nil {
		return
	}
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	startedAt := now()
	r.tokenMu.Lock()
	if !r.tokenNextRun.IsZero() && r.tokenNextRun.After(startedAt) {
		r.tokenMu.Unlock()
		return
	}
	r.tokenNextRun = startedAt.Add(upstreamPricePageRetryInterval)
	r.tokenMu.Unlock()

	ctx, cancel := context.WithTimeout(r.ctx, upstreamPricePageSyncTimeout)
	result, syncErr := r.monitor.SyncTokenPrices(ctx)
	cancel()
	nextInterval := upstreamPricePageSyncInterval
	if syncErr != nil {
		nextInterval = upstreamPricePageRetryInterval
		slog.Warn("upstream_token_price_sync_failed", "error", syncErr)
	} else if result != nil && result.ChangedModels > 0 {
		slog.Info("upstream_token_price_sync_applied",
			"source_models", result.SourceModels,
			"models", result.Models,
			"changed_models", result.ChangedModels,
			"changed_channel_rows", result.ChangedChannelRows,
			"changed_interval_rows", result.ChangedIntervalRows,
			"changed_display_rows", result.ChangedDisplayRows,
			"created_display_rows", result.CreatedDisplayRows)
	}
	r.tokenMu.Lock()
	r.tokenNextRun = now().Add(nextInterval)
	r.tokenMu.Unlock()
}

func (r *UpstreamPriceMonitorRunner) runPerRequestIfDue() {
	if r == nil || r.monitor == nil {
		return
	}
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	startedAt := now()
	r.perRequestMu.Lock()
	if !r.perRequestNextRun.IsZero() && r.perRequestNextRun.After(startedAt) {
		r.perRequestMu.Unlock()
		return
	}
	// Reserve the retry window before doing I/O. Concurrent callers therefore
	// collapse into this one attempt even if tests or future callers invoke the
	// scheduler outside its normal single goroutine.
	r.perRequestNextRun = startedAt.Add(upstreamPricePageRetryInterval)
	r.perRequestMu.Unlock()

	ctx, cancel := context.WithTimeout(r.ctx, upstreamPricePageSyncTimeout)
	result, perRequestErr := r.monitor.SyncPerRequestPrices(ctx)
	cancel()

	nextInterval := upstreamPricePageSyncInterval
	if perRequestErr != nil {
		nextInterval = upstreamPricePageRetryInterval
		if perRequestErr != nil {
			slog.Warn("upstream_per_request_price_sync_failed", "error", perRequestErr)
		}
	} else if result != nil && result.ChangedModels > 0 {
		slog.Info("upstream_per_request_price_sync_applied",
			"models", result.Models,
			"changed_models", result.ChangedModels,
			"changed_channel_rows", result.ChangedChannelRows)
	}
	r.perRequestMu.Lock()
	r.perRequestNextRun = now().Add(nextInterval)
	r.perRequestMu.Unlock()
}
