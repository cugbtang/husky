package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/husky/husky/pkg/llm"
)

const (
	// defaultReActMaxRounds 限制单个步骤的「思考 → 行动」轮数，防止模型在工具间空转。
	defaultReActMaxRounds = 10
	// defaultReActHistoryWindow 只把最近若干轮观察回灌给模型，控制 prompt 体积。
	defaultReActHistoryWindow = 5
	// maxReActRawEchoLength 回灌解析失败原文的长度上限，避免一次乱码输出吃掉整窗上下文。
	maxReActRawEchoLength = 500
)

// reactToolNames 是提示词与错误反馈里使用的工具清单，保持单一来源。
var reactToolNames = []string{"search_kb", "reply_user", "update_ticket", "complete", "fail"}

// reactToolset 是 ReAct 循环需要的外部副作用集合。
//
// 上游把循环直接写在 WorkflowService 方法里，DB（ticketRepo/agentRepo）与 LLM
// 都是具体类型，导致循环逻辑只能靠起一个真实 PG + 真实模型才能验证。抽成接口后，
// 循环本身变成纯编排：给定一串模型输出与一组假工具，就能断言每一轮的走向。
type reactToolset interface {
	Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error)
	SearchKB(ctx context.Context, query string) string
	ReplyUser(ctx context.Context, msg string)
	UpdateTicket(ctx context.Context, input string) string
	CompleteStep(ctx context.Context, summary string)
	FailStep(ctx context.Context, reason string)
}

// reactRound 记录一轮「行动 → 观察」，下一轮会回灌给模型。
type reactRound struct {
	Action ReActAction
	Result string
}

// reactLoopConfig 是一次 ReAct 执行所需的只读参数。
type reactLoopConfig struct {
	StepName       string
	Goal           string
	TicketTitle    string
	TicketStatus   string
	TicketPriority string
	Description    string
	KBContext      string
	Model          string
	Temperature    float32
	MaxTokens      int
	MaxRounds      int
	HistoryWindow  int
}

// reactLoopOutcome 表示循环的结束方式，便于调用方与测试断言。
type reactLoopOutcome string

const (
	// reactOutcomeCompleted 模型主动调用 complete。
	reactOutcomeCompleted reactLoopOutcome = "completed"
	// reactOutcomeFailed 模型调用 fail，或 LLM 调用本身报错。
	reactOutcomeFailed reactLoopOutcome = "failed"
	// reactOutcomeExhausted 轮数耗尽，按上游语义自动完成步骤。
	reactOutcomeExhausted reactLoopOutcome = "exhausted"
)

// runReActLoop 执行 ReAct 循环，直到模型调用 complete/fail、LLM 报错或轮数耗尽。
// 它只做编排与决策，所有副作用都通过 tools 完成。
func runReActLoop(ctx context.Context, cfg reactLoopConfig, tools reactToolset) reactLoopOutcome {
	maxRounds := cfg.MaxRounds
	if maxRounds <= 0 {
		maxRounds = defaultReActMaxRounds
	}
	window := cfg.HistoryWindow
	if window <= 0 {
		window = defaultReActHistoryWindow
	}

	systemPrompt := buildReActSystemPrompt(cfg)
	var history []reactRound

	for roundIdx := 0; roundIdx < maxRounds; roundIdx++ {
		resp, err := tools.Chat(ctx, &llm.ChatRequest{
			Model:       cfg.Model,
			Temperature: cfg.Temperature,
			MaxTokens:   cfg.MaxTokens,
			Messages: []llm.ChatMessage{
				{Role: "system", Content: systemPrompt},
				{Role: "user", Content: buildReActUserMessage(cfg, history, window)},
			},
		})
		if err != nil {
			tools.FailStep(ctx, fmt.Sprintf("LLM error: %v", err))
			return reactOutcomeFailed
		}
		if resp == nil {
			tools.FailStep(ctx, "LLM error: empty response")
			return reactOutcomeFailed
		}

		action, parseErr := parseReActAction(resp.Content)
		if parseErr != nil {
			// 不合格输出不消耗步数决策，只是把错误回灌，让模型自我纠正。
			history = append(history, reactRound{
				Action: ReActAction{
					Thought:     "parse_error",
					Action:      "invalid_json",
					ActionInput: truncateForEcho(resp.Content),
				},
				Result: fmt.Sprintf("Your response was not valid JSON (%v). Respond only with the JSON format.", parseErr),
			})
			continue
		}

		log.Printf("ReAct [%s] round %d: %s -> %s(%s)", cfg.StepName, roundIdx+1, action.Thought, action.Action, action.ActionInput)

		switch action.Action {
		case "search_kb":
			history = append(history, reactRound{Action: action, Result: tools.SearchKB(ctx, action.ActionInput)})
		case "reply_user":
			tools.ReplyUser(ctx, action.ActionInput)
			history = append(history, reactRound{Action: action, Result: "replied to user"})
		case "update_ticket":
			history = append(history, reactRound{Action: action, Result: tools.UpdateTicket(ctx, action.ActionInput)})
		case "complete":
			tools.CompleteStep(ctx, action.ActionInput)
			return reactOutcomeCompleted
		case "fail":
			tools.FailStep(ctx, action.ActionInput)
			return reactOutcomeFailed
		default:
			history = append(history, reactRound{
				Action: action,
				Result: fmt.Sprintf("unknown tool %q, use one of: %s", action.Action, strings.Join(reactToolNames, ", ")),
			})
		}
	}

	tools.CompleteStep(ctx, "reached max rounds, auto-completed")
	return reactOutcomeExhausted
}

// parseReActAction 把模型输出解析成 ReActAction。
//
// 上游直接对整段输出 json.Unmarshal，只要模型多写一句 "Here is my action:" 或者
// 习惯性地套一层 ```json 代码块，就会解析失败——然后重试、再失败、直到轮数耗尽，
// 步骤被 "reached max rounds, auto-completed" 静默标记为完成（实际上什么都没做）。
// 这里做三件事：剥代码块围栏、截取第一个完整 JSON 对象、校验 action 非空。
func parseReActAction(raw string) (ReActAction, error) {
	var action ReActAction
	text := strings.TrimSpace(raw)
	if text == "" {
		return action, fmt.Errorf("empty response")
	}

	text = stripCodeFence(text)
	payload := extractFirstJSONObject(text)
	if payload == "" {
		return action, fmt.Errorf("no JSON object found")
	}

	if err := json.Unmarshal([]byte(payload), &action); err != nil {
		return action, fmt.Errorf("invalid JSON: %w", err)
	}
	action.Action = strings.TrimSpace(action.Action)
	if action.Action == "" {
		return action, fmt.Errorf("missing action field")
	}
	return action, nil
}

// stripCodeFence 去掉 ```json ... ``` / ``` ... ``` 包裹。
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	idx := strings.IndexByte(s, '\n')
	if idx < 0 {
		return s
	}
	s = s[idx+1:]
	if end := strings.LastIndex(s, "```"); end >= 0 {
		s = s[:end]
	}
	return strings.TrimSpace(s)
}

// extractFirstJSONObject 从任意文本里截出第一个花括号平衡的 JSON 对象。
// 会跳过字符串字面量里的花括号与转义引号，因此 action_input 里含 JSON 也能正确截断。
func extractFirstJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// buildReActSystemPrompt 生成系统提示词。
func buildReActSystemPrompt(cfg reactLoopConfig) string {
	return fmt.Sprintf(`You are an AI agent executing SOP steps. You MUST respond with valid JSON only:
{"thought":"...","action":"tool_name","action_input":"..."}

Tools:
- search_kb: Search knowledge base. Input: search query string
- reply_user: Reply to user. Input: your response text
- update_ticket: Update ticket status or priority. Input: JSON like {"status":"resolved"}
- complete: Mark this step done. Input: result summary
- fail: Mark this step failed. Input: reason

Step: %s
Goal: %s
Ticket: %s | Status: %s | Priority: %s
Description: %s

Relevant knowledge:
%s

Stay focused. When goal achieved, use "complete".`, cfg.StepName, cfg.Goal, cfg.TicketTitle, cfg.TicketStatus, cfg.TicketPriority, cfg.Description, cfg.KBContext)
}

// buildReActUserMessage 生成每轮的 user 消息：当前状态 + 最近 window 轮观察。
func buildReActUserMessage(cfg reactLoopConfig, history []reactRound, window int) string {
	return fmt.Sprintf("Current state: step=%s, ticket_status=%s\nHistory:\n%s\n\nWhat should I do next?",
		cfg.StepName, cfg.TicketStatus, buildReActHistory(history, window))
}

// buildReActHistory 把历史轮次渲染成文本，只保留最近 window 轮（window<=0 表示不裁剪）。
func buildReActHistory(history []reactRound, window int) string {
	start := 0
	if window > 0 && len(history) > window {
		start = len(history) - window
	}
	var b strings.Builder
	for i := start; i < len(history); i++ {
		h := history[i]
		fmt.Fprintf(&b, "Round %d: thought=%s action=%s input=%s\n  -> %s\n",
			i+1, h.Action.Thought, h.Action.Action, h.Action.ActionInput, h.Result)
	}
	return b.String()
}

// truncateForEcho 截断回灌原文，避免超长乱码输出挤掉正常上下文。
func truncateForEcho(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxReActRawEchoLength {
		return s
	}
	return s[:maxReActRawEchoLength] + "...(truncated)"
}

// formatQueuedUserMessages 把队列里积攒的用户消息拼进 step.Result。
// 上游只取了 messages[0]，一批涌入多条消息时其余会被静默丢弃。
func formatQueuedUserMessages(messages []string) string {
	if len(messages) == 0 {
		return ""
	}
	return "\n[User]: " + strings.Join(messages, "\n[User]: ")
}
