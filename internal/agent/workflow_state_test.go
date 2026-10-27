package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/husky/husky/internal/model"
)

func uintPtr(v uint) *uint { return &v }

func TestEvaluateCondition(t *testing.T) {
	ticket := &model.Ticket{
		Status:     "in_progress",
		Priority:   "high",
		AssigneeID: uintPtr(7),
	}
	unassigned := &model.Ticket{Status: "open", Priority: "low"}

	tests := []struct {
		name    string
		cfg     conditionConfig
		ticket  *model.Ticket
		want    bool
		wantErr string
	}{
		{
			name:   "status eq 命中",
			cfg:    conditionConfig{Field: "status", Operator: "eq", Value: "in_progress"},
			ticket: ticket,
			want:   true,
		},
		{
			name:   "status eq 不命中",
			cfg:    conditionConfig{Field: "status", Operator: "eq", Value: "resolved"},
			ticket: ticket,
			want:   false,
		},
		{
			name:   "status ne 命中",
			cfg:    conditionConfig{Field: "status", Operator: "ne", Value: "resolved"},
			ticket: ticket,
			want:   true,
		},
		{
			name:   "priority eq 命中",
			cfg:    conditionConfig{Field: "priority", Operator: "eq", Value: "high"},
			ticket: ticket,
			want:   true,
		},
		{
			name:   "priority ne 不命中",
			cfg:    conditionConfig{Field: "priority", Operator: "ne", Value: "high"},
			ticket: ticket,
			want:   false,
		},
		{
			name:   "assignee_id ne null 命中（上游支持的唯一一种写法）",
			cfg:    conditionConfig{Field: "assignee_id", Operator: "ne", Value: nil},
			ticket: ticket,
			want:   true,
		},
		{
			name:   "assignee_id ne null 未分配时不命中",
			cfg:    conditionConfig{Field: "assignee_id", Operator: "ne", Value: nil},
			ticket: unassigned,
			want:   false,
		},
		{
			name:   "assignee_id eq null 未分配时命中",
			cfg:    conditionConfig{Field: "assignee_id", Operator: "eq", Value: nil},
			ticket: unassigned,
			want:   true,
		},
		{
			name:   "assignee_id eq 具体 ID（JSON 数字解成 float64）",
			cfg:    conditionConfig{Field: "assignee_id", Operator: "eq", Value: float64(7)},
			ticket: ticket,
			want:   true,
		},
		{
			name:   "assignee_id eq 另一个 ID 不命中",
			cfg:    conditionConfig{Field: "assignee_id", Operator: "eq", Value: float64(9)},
			ticket: ticket,
			want:   false,
		},
		{
			name:   "assignee_id ne 具体 ID 命中",
			cfg:    conditionConfig{Field: "assignee_id", Operator: "ne", Value: float64(9)},
			ticket: ticket,
			want:   true,
		},
		{
			name:   "assignee_id ne 具体 ID 但工单未分配：视为不相等",
			cfg:    conditionConfig{Field: "assignee_id", Operator: "ne", Value: float64(9)},
			ticket: unassigned,
			want:   true,
		},
		{
			name:   "assignee_id 值支持 int",
			cfg:    conditionConfig{Field: "assignee_id", Operator: "eq", Value: 7},
			ticket: ticket,
			want:   true,
		},
		{
			name:    "status 的值不是字符串时报错（上游会静默返回 false）",
			cfg:     conditionConfig{Field: "status", Operator: "eq", Value: float64(1)},
			ticket:  ticket,
			wantErr: "must be a string",
		},
		{
			name:    "不认识的字段报错",
			cfg:     conditionConfig{Field: "sla_status", Operator: "eq", Value: "breached"},
			ticket:  ticket,
			wantErr: "unsupported condition field",
		},
		{
			name:    "不认识的运算符报错（上游会静默不命中）",
			cfg:     conditionConfig{Field: "status", Operator: "contains", Value: "progress"},
			ticket:  ticket,
			wantErr: "unsupported operator",
		},
		{
			name:    "缺运算符报错",
			cfg:     conditionConfig{Field: "priority", Value: "high"},
			ticket:  ticket,
			wantErr: "missing operator",
		},
		{
			name:    "assignee_id 的运算符也在白名单内",
			cfg:     conditionConfig{Field: "assignee_id", Operator: "gt", Value: float64(1)},
			ticket:  ticket,
			wantErr: "unsupported operator",
		},
		{
			name:    "assignee_id 的值不能是字符串",
			cfg:     conditionConfig{Field: "assignee_id", Operator: "eq", Value: "7"},
			ticket:  ticket,
			wantErr: "must be a number or null",
		},
		{
			name:    "ticket 为 nil 报错",
			cfg:     conditionConfig{Field: "status", Operator: "eq", Value: "open"},
			ticket:  nil,
			wantErr: "ticket is nil",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := evaluateCondition(tt.cfg, tt.ticket)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want 包含 %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tt.want {
				t.Errorf("matched = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHumanStepTimeoutMinutes(t *testing.T) {
	tests := []struct {
		name string
		cfg  humanStepConfig
		want int
	}{
		{"未配置走缺省 24 小时", humanStepConfig{}, 1440},
		{"显式配置生效", humanStepConfig{TimeoutMinutes: 30}, 30},
		{"负数回落到缺省", humanStepConfig{TimeoutMinutes: -5}, 1440},
	}

	for _, tt := range tests {
		if got := humanStepTimeoutMinutes(tt.cfg); got != tt.want {
			t.Errorf("%s: got %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestHumanStepStillWaiting(t *testing.T) {
	waiting := []string{"running", "pending"}
	finished := []string{"completed", "failed", "skipped", ""}

	for _, status := range waiting {
		if !humanStepStillWaiting(status) {
			t.Errorf("状态 %q 仍在等人工处理，应判定为可超时", status)
		}
	}
	for _, status := range finished {
		if humanStepStillWaiting(status) {
			t.Errorf("状态 %q 已结束，不该再判超时", status)
		}
	}
}

// TestHumanStepTimeOutPredicateRegression 记录上游那个让超时彻底失效的谓词。
//
// 上游写法：到点后 `if saved.Status != "pending" { return }`。
// 但 executeHumanStep 在启动定时器之前就把状态置成了 running，所以到点时状态
// 一定是 running —— 谓词恒成立，超时判定永远不会执行。
func TestHumanStepTimeOutPredicateRegression(t *testing.T) {
	upstreamWouldAbort := func(status string) bool { return status != "pending" }

	// 定时器到点时步骤的真实状态
	const statusAtTimeout = "running"

	if !upstreamWouldAbort(statusAtTimeout) {
		t.Fatal("预期上游谓词在 running 时成立（即放弃超时判定）")
	}
	if !humanStepStillWaiting(statusAtTimeout) {
		t.Fatal("修复后 running 状态必须判定为可超时")
	}
}

func TestIsLastStep(t *testing.T) {
	tests := []struct {
		stepIndex  int
		totalSteps int
		want       bool
	}{
		{0, 3, false},
		{1, 3, false},
		{2, 3, true},
		{5, 3, true}, // 脏数据下越界索引仍视为最后一步
		{0, 1, true},
		{0, 0, false},  // TotalSteps 缺失时不能把第一步当成最后一步
		{0, -1, false}, // 同上
	}

	for _, tt := range tests {
		if got := isLastStep(tt.stepIndex, tt.totalSteps); got != tt.want {
			t.Errorf("isLastStep(%d, %d) = %v, want %v", tt.stepIndex, tt.totalSteps, got, tt.want)
		}
	}
}

func TestHumanStepTimers(t *testing.T) {
	var timers humanStepTimers

	cancelled := make(map[uint]int)
	var mu sync.Mutex
	mk := func(id uint) context.CancelFunc {
		return func() {
			mu.Lock()
			cancelled[id]++
			mu.Unlock()
		}
	}

	timers.register(1, mk(1))
	timers.register(2, mk(2))
	if got := timers.pending(); got != 2 {
		t.Fatalf("注册两个定时器后 pending = %d, want 2", got)
	}

	timers.cancel(1)
	if got := timers.pending(); got != 1 {
		t.Errorf("取消后 pending = %d, want 1", got)
	}
	if cancelled[1] != 1 {
		t.Errorf("cancel(1) 应调用一次 cancel func, got %d", cancelled[1])
	}

	// 重复取消同一个 ID 不应 panic，也不应重复调用
	timers.cancel(1)
	if cancelled[1] != 1 {
		t.Errorf("重复取消不该重复调用 cancel func, got %d", cancelled[1])
	}

	// 取消不存在的 ID 是空操作
	timers.cancel(999)
	if cancelled[999] != 0 {
		t.Errorf("取消不存在的 ID 不应触发任何 cancel func")
	}

	// 同一步骤重复注册（例如打回重跑）会覆盖旧定时器，旧的不再被追踪
	timers.register(2, mk(2))
	if got := timers.pending(); got != 1 {
		t.Errorf("重复注册同一 ID 后 pending = %d, want 1", got)
	}
	timers.cancel(2)
	if cancelled[2] != 1 {
		t.Errorf("应只调用最新的 cancel func 一次, got %d", cancelled[2])
	}

	if got := timers.pending(); got != 0 {
		t.Errorf("全部取消后 pending = %d, want 0", got)
	}

	// 并发注册 / 取消不应 panic 或丢状态
	var wg sync.WaitGroup
	for i := uint(100); i < 200; i++ {
		wg.Add(1)
		go func(id uint) {
			defer wg.Done()
			timers.register(id, mk(id))
		}(i)
	}
	wg.Wait()
	if got := timers.pending(); got != 100 {
		t.Fatalf("并发注册后 pending = %d, want 100", got)
	}
	for i := uint(100); i < 200; i++ {
		wg.Add(1)
		go func(id uint) {
			defer wg.Done()
			timers.cancel(id)
		}(i)
	}
	wg.Wait()
	if got := timers.pending(); got != 0 {
		t.Errorf("并发取消后 pending = %d, want 0", got)
	}
}
