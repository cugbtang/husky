package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/husky/husky/internal/model"
)

// defaultHumanStepTimeoutMinutes 是人工步骤缺省超时：24 小时。
const defaultHumanStepTimeoutMinutes = 1440

// conditionConfig 是 condition 类型步骤的配置。
type conditionConfig struct {
	Field    string      `json:"field"`
	Operator string      `json:"operator"`
	Value    interface{} `json:"value"`
}

// humanStepConfig 是 human 类型步骤的配置。
type humanStepConfig struct {
	TimeoutMinutes int `json:"timeout_minutes"`
}

// evaluateCondition 判断 condition 步骤是否命中。
//
// 上游的写法是 `ticket.Status == cfg.Value` 这样直接比 interface{}：值类型不对时
// 不会报错，只是安静地返回 false，条件步骤就静默走了「不命中」分支——分支走错
// 比报错难查得多。这里改成显式类型断言 + 操作符白名单，不认识就返回错误。
func evaluateCondition(cfg conditionConfig, ticket *model.Ticket) (bool, error) {
	if ticket == nil {
		return false, fmt.Errorf("ticket is nil")
	}
	switch cfg.Field {
	case "status":
		return compareStringField("status", ticket.Status, cfg)
	case "priority":
		return compareStringField("priority", ticket.Priority, cfg)
	case "assignee_id":
		return compareAssigneeField(ticket.AssigneeID, cfg)
	default:
		return false, fmt.Errorf("unsupported condition field %q (supported: status, priority, assignee_id)", cfg.Field)
	}
}

func compareStringField(field, got string, cfg conditionConfig) (bool, error) {
	want, ok := cfg.Value.(string)
	if !ok {
		return false, fmt.Errorf("condition value for %q must be a string, got %T", field, cfg.Value)
	}
	switch cfg.Operator {
	case "eq":
		return got == want, nil
	case "ne":
		return got != want, nil
	case "":
		return false, fmt.Errorf("condition on %q is missing operator", field)
	default:
		return false, fmt.Errorf("unsupported operator %q for field %q (supported: eq, ne)", cfg.Operator, field)
	}
}

func compareAssigneeField(assignee *uint, cfg conditionConfig) (bool, error) {
	if cfg.Operator != "eq" && cfg.Operator != "ne" {
		return false, fmt.Errorf("unsupported operator %q for field \"assignee_id\" (supported: eq, ne)", cfg.Operator)
	}

	var wantNil bool
	var wantID uint
	switch v := cfg.Value.(type) {
	case nil:
		wantNil = true
	case float64: // encoding/json 把数字解成 float64
		wantID = uint(v)
	case int:
		wantID = uint(v)
	case string:
		return false, fmt.Errorf("condition value for \"assignee_id\" must be a number or null, got string")
	default:
		return false, fmt.Errorf("condition value for \"assignee_id\" must be a number or null, got %T", cfg.Value)
	}

	if wantNil {
		if cfg.Operator == "eq" {
			return assignee == nil, nil
		}
		return assignee != nil, nil
	}
	if assignee == nil {
		// 没有负责人时，「等于某个 ID」为假，「不等于某个 ID」为真。
		return cfg.Operator == "ne", nil
	}
	if cfg.Operator == "eq" {
		return *assignee == wantID, nil
	}
	return *assignee != wantID, nil
}

// humanStepTimeoutMinutes 返回人工步骤的超时分钟数，非正数回落到缺省值。
func humanStepTimeoutMinutes(cfg humanStepConfig) int {
	if cfg.TimeoutMinutes <= 0 {
		return defaultHumanStepTimeoutMinutes
	}
	return cfg.TimeoutMinutes
}

// humanStepStillWaiting 判断超时到点时步骤是否仍在等人工处理。
//
// 上游判断的是 `saved.Status != "pending"` 就直接返回——但 executeHumanStep 在启动
// 定时器之前就已经把状态置成了 running，所以这个条件恒为真，整段超时处理是死代码：
// 人工步骤永远不会超时。真正该判断的是「是否还在 running/pending」。
func humanStepStillWaiting(status string) bool {
	return status == "running" || status == "pending"
}

// isLastStep 判断某一步是否是工作流的最后一步。
// totalSteps <= 0（脏数据）时不认为任何一步是最后一步，避免工作流被提前标记完成。
func isLastStep(stepIndex, totalSteps int) bool {
	if totalSteps <= 0 {
		return false
	}
	return stepIndex >= totalSteps-1
}

// humanStepTimers 管理人工步骤的超时定时器。
//
// 除了「注册 + 取消」，它还负责一件事：ReviseStep 把步骤打回重跑时必须取消上一个
// 定时器，否则旧定时器到点后会把新一轮尝试判成超时。上游用裸 sync.Map 存 cancel
// 函数，三处取消逻辑各写一遍，ReviseStep 那处漏了。
type humanStepTimers struct {
	mu sync.Mutex
	m  map[uint]context.CancelFunc
}

func (t *humanStepTimers) register(stepID uint, cancel context.CancelFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = make(map[uint]context.CancelFunc)
	}
	t.m[stepID] = cancel
}

func (t *humanStepTimers) cancel(stepID uint) {
	t.mu.Lock()
	cancel, ok := t.m[stepID]
	if ok {
		delete(t.m, stepID)
	}
	t.mu.Unlock()
	if ok && cancel != nil {
		cancel()
	}
}

// pending 返回当前登记在册的定时器数量，仅供测试断言。
func (t *humanStepTimers) pending() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}
