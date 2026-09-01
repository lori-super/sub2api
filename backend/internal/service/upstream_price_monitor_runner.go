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
	now               func() time.Time
}

var (
	upstreamPerRequestPriceSyncInterval  = 24 * time.Hour
	upstreamPerRequestPriceRetryInterval = time.Hour
	upstreamPerRequestPriceSyncTimeout   = time.Minute
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
		r.wg.Add(1)
		go r.loop()
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

func (r *UpstreamPriceMonitorRunner) runIfDue() {
	// Public per-request pricing has its own daily clock and does not inherit
	// token monitoring's enabled/mode state.
	r.runPerRequestIfDue()

	// Nineteen models can require up to seven settled ledger samples each. The
	// service has a tighter 45-minute deadline; this outer allowance leaves time
	// to persist the terminal run state.
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
		DryRun:  cfg.Mode != domain.UpstreamPriceMonitorModeAutoApply,
	})
	if err != nil && !errors.Is(err, ErrUpstreamPriceMonitorRunConflict) {
		slog.Warn("upstream_price_monitor_run_failed", "error", err)
	}
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
	r.perRequestNextRun = startedAt.Add(upstreamPerRequestPriceRetryInterval)
	r.perRequestMu.Unlock()

	ctx, cancel := context.WithTimeout(r.ctx, upstreamPerRequestPriceSyncTimeout)
	result, perRequestErr := r.monitor.SyncPerRequestPrices(ctx)
	cancel()

	var tokenDisplayErr error
	var tokenDisplayResult *DisplayUpstreamTokenPriceSyncResult
	if tokenFetcher, ok := r.monitor.pricePage.(UpstreamTokenPricePageFetcher); ok && r.monitor.displayPricing != nil {
		tokenCtx, tokenCancel := context.WithTimeout(r.ctx, upstreamPerRequestPriceSyncTimeout)
		tokenDisplayResult, tokenDisplayErr = r.monitor.displayPricing.SyncUpstreamTokenDisplayPrices(tokenCtx, tokenFetcher)
		tokenCancel()
	}
	nextInterval := upstreamPerRequestPriceSyncInterval
	if perRequestErr != nil || tokenDisplayErr != nil {
		nextInterval = upstreamPerRequestPriceRetryInterval
		if perRequestErr != nil {
			slog.Warn("upstream_per_request_price_sync_failed", "error", perRequestErr)
		}
		if tokenDisplayErr != nil {
			slog.Warn("upstream_token_display_price_sync_failed", "error", tokenDisplayErr)
		}
	} else if result != nil && result.ChangedModels > 0 {
		slog.Info("upstream_per_request_price_sync_applied",
			"models", result.Models,
			"changed_models", result.ChangedModels,
			"changed_channel_rows", result.ChangedChannelRows)
	}
	if tokenDisplayResult != nil && tokenDisplayResult.ChangedModels > 0 {
		slog.Info("upstream_token_display_price_sync_applied",
			"models", tokenDisplayResult.UpdatedModels,
			"changed_models", tokenDisplayResult.ChangedModels)
	}
	r.perRequestMu.Lock()
	r.perRequestNextRun = now().Add(nextInterval)
	r.perRequestMu.Unlock()
}
