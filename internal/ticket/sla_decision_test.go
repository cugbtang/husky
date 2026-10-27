package ticket

import (
	"testing"
	"time"

	"github.com/husky/husky/internal/model"
)

func TestDecideSLAAction(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	// 480 分钟处理时长 + 0.8 阈值 => 最后 96 分钟进入 warning
	const resolution = 480.0
	const threshold = 0.8

	dueAt := func(d time.Duration) *time.Time {
		t := now.Add(d)
		return &t
	}
	ticket := func(status, slaStatus string, due *time.Time) *model.Ticket {
		return &model.Ticket{Status: status, SLAStatus: slaStatus, DueAt: due}
	}

	tests := []struct {
		name       string
		ticket     *model.Ticket
		resolution float64
		threshold  float64
		wantAction SLAAction
		wantOK     bool
	}{
		{
			name:       "无截止时间不处理",
			ticket:     ticket("open", "normal", nil),
			resolution: resolution, threshold: threshold,
			wantOK: false,
		},
		{
			name:       "已解决工单即使超时也不升级",
			ticket:     ticket("resolved", "normal", dueAt(-time.Hour)),
			resolution: resolution, threshold: threshold,
			wantOK: false,
		},
		{
			name:       "已关闭工单即使超时也不升级",
			ticket:     ticket("closed", "normal", dueAt(-time.Hour)),
			resolution: resolution, threshold: threshold,
			wantOK: false,
		},
		{
			name:       "超时且未标记 => 升级为 breached",
			ticket:     ticket("open", "normal", dueAt(-time.Minute)),
			resolution: resolution, threshold: threshold,
			wantAction: SLAAction{Status: "breached", Event: "sla_breach"}, wantOK: true,
		},
		{
			name:       "已 breached 不重复升级（防重复发通知）",
			ticket:     ticket("open", "breached", dueAt(-time.Hour)),
			resolution: resolution, threshold: threshold,
			wantOK: false,
		},
		{
			name:       "进入 warning 窗口 => 预警",
			ticket:     ticket("open", "normal", dueAt(30*time.Minute)),
			resolution: resolution, threshold: threshold,
			wantAction: SLAAction{Status: "warning", Event: "sla_warning"}, wantOK: true,
		},
		{
			name:       "warning 已置位不重复预警",
			ticket:     ticket("open", "warning", dueAt(30*time.Minute)),
			resolution: resolution, threshold: threshold,
			wantOK: false,
		},
		{
			name:       "warning 状态但已超时 => 继续升级到 breached",
			ticket:     ticket("open", "warning", dueAt(-time.Minute)),
			resolution: resolution, threshold: threshold,
			wantAction: SLAAction{Status: "breached", Event: "sla_breach"}, wantOK: true,
		},
		{
			name:       "距截止还远 => 不动",
			ticket:     ticket("open", "normal", dueAt(6*time.Hour)),
			resolution: resolution, threshold: threshold,
			wantOK: false,
		},
		{
			name:       "非法阈值回退到默认 0.8",
			ticket:     ticket("open", "normal", dueAt(30*time.Minute)),
			resolution: resolution, threshold: 1.5,
			wantAction: SLAAction{Status: "warning", Event: "sla_warning"}, wantOK: true,
		},
		{
			name:       "处理时长非法回退到 480 分钟",
			ticket:     ticket("open", "normal", dueAt(-time.Minute)),
			resolution: 0, threshold: threshold,
			wantAction: SLAAction{Status: "breached", Event: "sla_breach"}, wantOK: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := DecideSLAAction(tc.ticket, tc.resolution, tc.threshold, now)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.wantAction {
				t.Fatalf("action = %+v, want %+v", got, tc.wantAction)
			}
		})
	}
}

func TestDecideSLAActionNilTicket(t *testing.T) {
	if _, ok := DecideSLAAction(nil, 480, 0.8, time.Now()); ok {
		t.Fatal("nil ticket should not produce an action")
	}
}

func TestSLAEscalatorInterval(t *testing.T) {
	e := NewSLAEscalator(nil, nil)
	if e.Interval() != 5*time.Minute {
		t.Fatalf("default interval = %v, want 5m", e.Interval())
	}
	e.SetInterval(time.Minute)
	if e.Interval() != time.Minute {
		t.Fatalf("interval = %v, want 1m", e.Interval())
	}
}
