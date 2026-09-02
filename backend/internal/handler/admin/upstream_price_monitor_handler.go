package admin

import (
	"context"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type UpstreamPriceMonitorHandler struct {
	monitor *service.UpstreamPriceMonitorService
}

func NewUpstreamPriceMonitorHandler(monitor *service.UpstreamPriceMonitorService) *UpstreamPriceMonitorHandler {
	return &UpstreamPriceMonitorHandler{monitor: monitor}
}

func (h *UpstreamPriceMonitorHandler) GetConfig(c *gin.Context) {
	cfg, err := h.monitor.GetConfig(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, cfg)
}

func (h *UpstreamPriceMonitorHandler) UpdateConfig(c *gin.Context) {
	var cfg domain.UpstreamPriceMonitorConfig
	if err := c.ShouldBindJSON(&cfg); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := h.monitor.UpdateConfig(c.Request.Context(), &cfg); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, &cfg)
}

func (h *UpstreamPriceMonitorHandler) GetRuntime(c *gin.Context) {
	runtime, err := h.monitor.GetRuntime(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, runtime)
}

// SyncTokenPrices is the manual trigger for the same authoritative public-page
// transaction used by the runner. Keeping the legacy display-pricing URL while
// routing it here prevents an administrator action from updating presentation
// prices without the corresponding real channel prices.
func (h *UpstreamPriceMonitorHandler) SyncTokenPrices(c *gin.Context) {
	result, err := h.monitor.SyncTokenPrices(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *UpstreamPriceMonitorHandler) ListModels(c *gin.Context) {
	items, err := h.monitor.ListModelCatalog(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"items": items})
}

type updateUpstreamPriceModelStatusRequest struct {
	Model  string                          `json:"model" binding:"required"`
	Status domain.UpstreamPriceModelStatus `json:"status" binding:"required"`
}

func (h *UpstreamPriceMonitorHandler) UpdateModelStatus(c *gin.Context) {
	var req updateUpstreamPriceModelStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	item, err := h.monitor.SetModelCatalogStatus(c.Request.Context(), req.Model, req.Status)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *UpstreamPriceMonitorHandler) DiscoverModels(c *gin.Context) {
	items, err := h.monitor.DiscoverModelCatalog(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"items": items})
}

func (h *UpstreamPriceMonitorHandler) ListEvidence(c *gin.Context) {
	page, err := h.monitor.ListRuns(c.Request.Context(), 1, 0)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if page == nil || len(page.Items) == 0 {
		response.Success(c, gin.H{"items": []domain.UpstreamPriceEvidence{}})
		return
	}
	items, err := h.monitor.ListRunEvidence(c.Request.Context(), page.Items[0].ID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"items": items})
}

func (h *UpstreamPriceMonitorHandler) ListRuns(c *gin.Context) {
	pageNumber, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if pageNumber < 1 {
		pageNumber = 1
	}
	if pageSize < 1 || pageSize > 200 {
		pageSize = 20
	}
	status := domain.UpstreamPriceMonitorRunStatus(c.Query("status"))
	result, err := h.monitor.ListRuns(c.Request.Context(), pageSize, (pageNumber-1)*pageSize, status)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	items := make([]domain.UpstreamPriceMonitorRun, 0, len(result.Items))
	for i := range result.Items {
		items = append(items, *sanitizeUpstreamPriceMonitorRun(&result.Items[i]))
	}
	response.Success(c, gin.H{"items": items, "total": result.Total, "page": pageNumber, "page_size": pageSize})
}

type createUpstreamPriceMonitorRunRequest struct {
	DryRun *bool `json:"dry_run" binding:"required"`
}

func (h *UpstreamPriceMonitorHandler) CreateRun(c *gin.Context) {
	var req createUpstreamPriceMonitorRunRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if req.DryRun == nil || !*req.DryRun {
		response.BadRequest(c, "Manual upstream price monitor runs must use dry_run=true")
		return
	}
	// A full active-only round can take many minutes. Detach it from the HTTP
	// request and return as soon as its durable running row is visible; the UI
	// polls the existing runtime/runs endpoints for progress and completion.
	beforeID := int64(0)
	page, err := h.monitor.ListRuns(c.Request.Context(), 1, 0)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if page != nil && len(page.Items) > 0 {
		beforeID = page.Items[0].ID
	}
	runCtx, cancelRun := context.WithTimeout(context.Background(), 46*time.Minute)
	type runResult struct {
		run *domain.UpstreamPriceMonitorRun
		err error
	}
	resultCh := make(chan runResult, 1)
	go func() {
		defer cancelRun()
		run, err := h.monitor.RunOnce(runCtx, service.UpstreamPriceRunOptions{
			Trigger: domain.UpstreamPriceMonitorRunTriggerManual,
			DryRun:  true, // Collection never writes inline; review mode exposes a separate step-up Apply action.
		})
		resultCh <- runResult{run: run, err: err}
	}()

	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case result := <-resultCh:
			if result.err != nil && result.run == nil {
				response.ErrorFrom(c, result.err)
				return
			}
			response.Accepted(c, gin.H{
				"accepted": true, "poll_after_ms": 2000,
				"run": sanitizeUpstreamPriceMonitorRun(result.run),
			})
			return
		case <-ticker.C:
			page, err := h.monitor.ListRuns(c.Request.Context(), 1, 0)
			if err != nil || page == nil || len(page.Items) == 0 || page.Items[0].ID <= beforeID {
				continue
			}
			response.Accepted(c, gin.H{
				"accepted": true, "poll_after_ms": 2000,
				"run": sanitizeUpstreamPriceMonitorRun(&page.Items[0]),
			})
			return
		case <-deadline.C:
			cancelRun()
			response.ErrorFrom(c, service.ErrUpstreamPriceMonitorUnavailable)
			return
		}
	}
}

type upstreamPriceRunActionRequest struct {
	SnapshotHash string `json:"snapshot_hash" binding:"required"`
}

func (h *UpstreamPriceMonitorHandler) ApplyRun(c *gin.Context) {
	h.runAction(c, false)
}

func (h *UpstreamPriceMonitorHandler) RollbackRun(c *gin.Context) {
	h.runAction(c, true)
}

func (h *UpstreamPriceMonitorHandler) runAction(c *gin.Context, rollback bool) {
	runID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || runID <= 0 {
		response.BadRequest(c, "Invalid run ID")
		return
	}
	var req upstreamPriceRunActionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	var run *domain.UpstreamPriceMonitorRun
	if rollback {
		run, err = h.monitor.RollbackRun(c.Request.Context(), runID, req.SnapshotHash)
	} else {
		run, err = h.monitor.ApplyRun(c.Request.Context(), runID, req.SnapshotHash)
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, sanitizeUpstreamPriceMonitorRun(run))
}

func sanitizeUpstreamPriceMonitorRun(run *domain.UpstreamPriceMonitorRun) *domain.UpstreamPriceMonitorRun {
	if run == nil {
		return nil
	}
	out := *run
	if run.Summary != nil {
		out.Summary = make(map[string]any, len(run.Summary))
		for key, value := range run.Summary {
			if key != "account_ledger_hashes" && key != "account_identity_hashes" {
				out.Summary[key] = value
			}
		}
	}
	return &out
}
