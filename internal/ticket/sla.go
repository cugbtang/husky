package ticket

import (
	"context"
	"log"
	"time"

	"github.com/husky/husky/internal/model"
	"github.com/husky/husky/internal/repository"
)

// defaultResolutionMinutes 在工单未匹配到 SLA 配置时使用的兜底处理时长。
const defaultResolutionMinutes float64 = 480

// defaultWarningThreshold 兜底告警阈值：剩余 20% 时间时进入 warning。
const defaultWarningThreshold float64 = 0.8

// SLAAction 描述一次扫描中针对单个工单应当执行的状态变更。
type SLAAction struct {
	Status string // breached / warning
	Event  string // sla_breach / sla_warning
}

// DecideSLAAction 判断工单在 now 时刻是否需要变更 SLA 状态。
//
// 这是一个纯函数：不访问数据库、不发通知，只做决策，便于单测覆盖。
// 第二个返回值表示是否需要执行变更；返回 false 时调用方应完全跳过该工单。
func DecideSLAAction(t *model.Ticket, resolutionMinutes, warningThreshold float64, now time.Time) (SLAAction, bool) {
	if t == nil || t.DueAt == nil {
		return SLAAction{}, false
	}
	// 已关闭/已解决的工单不再升级
	if t.Status == "resolved" || t.Status == "closed" {
		return SLAAction{}, false
	}
	if resolutionMinutes <= 0 {
		resolutionMinutes = defaultResolutionMinutes
	}
	if warningThreshold <= 0 || warningThreshold > 1 {
		warningThreshold = defaultWarningThreshold
	}

	// 已判定超时的工单不再重复升级：否则每轮扫描都会重复发通知、重复打 webhook
	if now.After(*t.DueAt) {
		if t.SLAStatus == "breached" {
			return SLAAction{}, false
		}
		return SLAAction{Status: "breached", Event: "sla_breach"}, true
	}

	// 进入 warning 窗口：resolution * (1 - 阈值) 分钟前开始告警
	warningMinutes := resolutionMinutes * (1 - warningThreshold)
	if t.SLAStatus == "normal" && now.After(t.DueAt.Add(-time.Duration(warningMinutes)*time.Minute)) {
		return SLAAction{Status: "warning", Event: "sla_warning"}, true
	}

	return SLAAction{}, false
}

// SLAEscalator monitors ticket SLA deadlines and escalates breached tickets.
type SLAEscalator struct {
	ticketRepo *repository.TicketRepository
	slaSvc     *SLAConfigService
	interval   time.Duration
	ctx        context.Context
	cancel     context.CancelFunc
}

func NewSLAEscalator(ticketRepo *repository.TicketRepository, slaSvc *SLAConfigService) *SLAEscalator {
	return &SLAEscalator{
		ticketRepo: ticketRepo,
		slaSvc:     slaSvc,
		interval:   5 * time.Minute,
	}
}

func (e *SLAEscalator) SetInterval(d time.Duration) {
	e.interval = d
}

// Interval 返回当前扫描间隔。
func (e *SLAEscalator) Interval() time.Duration {
	return e.interval
}

// RunOnce 执行一轮扫描，返回本轮真正发生状态变更（升级）的工单数量。
//
// 它不持有 ticker，供外部调度器（cmd/worker）按自己的节奏调用；
// 进程内模式（API 内嵌）仍可通过 Start/Stop 使用内置 ticker。
func (e *SLAEscalator) RunOnce(ctx context.Context) (int, error) {
	overdue, err := e.ticketRepo.FindOverdueTickets(ctx, time.Now())
	if err != nil {
		return 0, err
	}

	escalated := 0
	for i := range overdue {
		changed, err := e.escalateTicket(ctx, &overdue[i])
		if err != nil {
			log.Printf("SLA escalator: failed to escalate ticket %d: %v", overdue[i].ID, err)
			continue
		}
		if changed {
			escalated++
		}
	}
	return escalated, nil
}

// Start begins the SLA monitoring loop in a background goroutine.
func (e *SLAEscalator) Start(ctx context.Context) {
	e.ctx, e.cancel = context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(e.interval)
		defer ticker.Stop()

		log.Printf("SLA escalator started (interval: %v)", e.interval)

		for {
			select {
			case <-e.ctx.Done():
				log.Println("SLA escalator stopped")
				return
			case <-ticker.C:
				e.checkAndEscalate(e.ctx)
			}
		}
	}()
}

// Stop gracefully stops the SLA escalator.
func (e *SLAEscalator) Stop() {
	if e.cancel != nil {
		e.cancel()
	}
}

func (e *SLAEscalator) checkAndEscalate(ctx context.Context) {
	escalated, err := e.RunOnce(ctx)
	if err != nil {
		log.Printf("SLA escalator: failed to query overdue tickets: %v", err)
		return
	}
	if escalated > 0 {
		log.Printf("SLA escalator: escalated %d ticket(s)", escalated)
	}
}

// escalateTicket 对单个工单做决策并落库，返回是否发生了变更。
func (e *SLAEscalator) escalateTicket(ctx context.Context, ticket *model.Ticket) (bool, error) {
	resolutionMinutes := defaultResolutionMinutes
	warningThreshold := defaultWarningThreshold
	if e.slaSvc != nil {
		if cfg, err := e.slaSvc.FindMatch(ctx, ticket.Priority, &ticket.CategoryID); err == nil && cfg != nil {
			resolutionMinutes = float64(cfg.ResolutionMinutes)
			warningThreshold = cfg.WarningThreshold
		}
	}

	action, ok := DecideSLAAction(ticket, resolutionMinutes, warningThreshold, time.Now())
	if !ok {
		return false, nil
	}

	if err := e.ticketRepo.UpdateFields(ctx, ticket.ID, map[string]interface{}{
		"sla_status": action.Status,
	}); err != nil {
		return false, err
	}

	ticket.SLAStatus = action.Status

	// Fire SLA event via service (notification + webhook + channel)
	if e.slaSvc != nil {
		e.slaSvc.FireSLAEvent(ctx, ticket, action.Event)
	}

	return true, nil
}

// ComputeAndSetDueAt 根据 SLA 配置计算并设置工单 DueAt
func (e *SLAEscalator) ComputeAndSetDueAt(ctx context.Context, ticket *model.Ticket) {
	if e.slaSvc == nil {
		return
	}
	dueAt := e.slaSvc.ComputeDueAt(ctx, ticket)
	if dueAt != nil {
		if err := e.ticketRepo.UpdateFields(ctx, ticket.ID, map[string]interface{}{
			"due_at": dueAt,
		}); err != nil {
			log.Printf("SLA: failed to set due_at for ticket %d: %v", ticket.ID, err)
		}
	}
}
