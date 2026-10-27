package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/husky/husky/pkg/llm"
)

// fakeReactToolset 用一串预设的模型输出驱动 ReAct 循环，并记录所有副作用调用。
// 它让循环可以在没有 PG、没有真实模型的情况下跑完整轨迹。
type fakeReactToolset struct {
	responses []string // 每轮依次返回的模型输出
	errAt     int      // 第 N 轮（1 基）让 Chat 返回错误，0 表示不报错
	chatErr   error    // 与 errAt 搭配的错误
	kbReply   string   // SearchKB 的固定观察值

	requests  []*llm.ChatRequest
	searches  []string
	replies   []string
	updates   []string
	completed []string
	failed    []string
}

func (f *fakeReactToolset) Chat(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	f.requests = append(f.requests, req)
	round := len(f.requests)
	if f.errAt == round {
		err := f.chatErr
		if err == nil {
			err = errors.New("boom")
		}
		return nil, err
	}
	idx := round - 1
	if idx >= len(f.responses) {
		// 预设输出用完时返回一个安全的「继续查」动作，用于验证轮数耗尽分支。
		return &llm.ChatResponse{Content: `{"thought":"still thinking","action":"search_kb","action_input":"more"}`}, nil
	}
	return &llm.ChatResponse{Content: f.responses[idx], Model: req.Model}, nil
}

func (f *fakeReactToolset) SearchKB(_ context.Context, query string) string {
	f.searches = append(f.searches, query)
	if f.kbReply != "" {
		return f.kbReply
	}
	return "Observation: No relevant knowledge found."
}

func (f *fakeReactToolset) ReplyUser(_ context.Context, msg string) {
	f.replies = append(f.replies, msg)
}

func (f *fakeReactToolset) UpdateTicket(_ context.Context, input string) string {
	f.updates = append(f.updates, input)
	return "updated: status=in_progress"
}

func (f *fakeReactToolset) CompleteStep(_ context.Context, summary string) {
	f.completed = append(f.completed, summary)
}

func (f *fakeReactToolset) FailStep(_ context.Context, reason string) {
	f.failed = append(f.failed, reason)
}

// lastUserMessage 取最后一次 Chat 请求里的 user 消息，用来断言回灌内容。
func (f *fakeReactToolset) lastUserMessage() string {
	if len(f.requests) == 0 {
		return ""
	}
	msgs := f.requests[len(f.requests)-1].Messages
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

func testReactConfig() reactLoopConfig {
	return reactLoopConfig{
		StepName:       "定位根因",
		Goal:           "给出根因结论",
		TicketTitle:    "VPN 连不上",
		TicketStatus:   "open",
		TicketPriority: "high",
		Description:    "用户反馈公司 VPN 拨号报 619",
		KBContext:      "- 手册: 619 通常是 1723 端口被运营商封禁\n",
		Model:          "gpt-4o-mini",
		Temperature:    0.2,
		MaxTokens:      1024,
		MaxRounds:      10,
		HistoryWindow:  defaultReActHistoryWindow,
	}
}

const (
	actSearch   = `{"thought":"先查手册","action":"search_kb","action_input":"619 错误"}`
	actComplete = `{"thought":"有结论了","action":"complete","action_input":"运营商封 1723，改走 UDP 1194"}`
	actFail     = `{"thought":"无法处理","action":"fail","action_input":"缺少日志权限"}`
)

func TestRunReActLoop(t *testing.T) {
	tests := []struct {
		name        string
		responses   []string
		errAt       int
		maxRounds   int
		window      int
		kbReply     string
		wantOutcome reactLoopOutcome
		wantRounds  int
		wantDone    int
		wantFail    int
		wantSearch  []string
		wantReply   int
		wantUpdate  int
		lastPrompt  []string // 最后一次请求的 user 消息应包含
		notInPrompt []string // 最后一次请求的 user 消息不应包含
	}{
		{
			name:        "第一轮就 complete",
			responses:   []string{actComplete},
			wantOutcome: reactOutcomeCompleted,
			wantRounds:  1,
			wantDone:    1,
		},
		{
			name:        "代码块包裹的 JSON 也能解析（回归：上游会一直重试到轮数耗尽）",
			responses:   []string{"```json\n" + actComplete + "\n```"},
			wantOutcome: reactOutcomeCompleted,
			wantRounds:  1,
			wantDone:    1,
		},
		{
			name:        "JSON 前后带解释文字也能解析",
			responses:   []string{"我打算这样做：\n" + actComplete},
			wantOutcome: reactOutcomeCompleted,
			wantRounds:  1,
			wantDone:    1,
		},
		{
			name:        "先查知识库再 complete，观察值回灌到下一轮",
			responses:   []string{actSearch, actComplete},
			kbReply:     "Observation:\n619: 运营商封 1723\n",
			wantOutcome: reactOutcomeCompleted,
			wantRounds:  2,
			wantDone:    1,
			wantSearch:  []string{"619 错误"},
			lastPrompt:  []string{"619: 运营商封 1723"},
		},
		{
			name:        "输出不是 JSON 时回灌纠错信息而不是直接失败",
			responses:   []string{"我觉得应该先看看日志", actComplete},
			wantOutcome: reactOutcomeCompleted,
			wantRounds:  2,
			wantDone:    1,
			lastPrompt:  []string{"not valid JSON"},
		},
		{
			name:        "JSON 合法但缺 action 字段，按解析失败处理",
			responses:   []string{`{"thought":"嗯"}`, actComplete},
			wantOutcome: reactOutcomeCompleted,
			wantRounds:  2,
			wantDone:    1,
			lastPrompt:  []string{"missing action"},
		},
		{
			name:        "未知工具名回灌可用工具清单",
			responses:   []string{`{"thought":"试试","action":"drop_table","action_input":"tickets"}`, actComplete},
			wantOutcome: reactOutcomeCompleted,
			wantRounds:  2,
			wantDone:    1,
			lastPrompt:  []string{"unknown tool", "drop_table", "search_kb, reply_user, update_ticket, complete, fail"},
		},
		{
			name:        "reply_user 与 update_ticket 各调一次",
			responses:   []string{`{"thought":"先安抚","action":"reply_user","action_input":"我们已在排查"}`, `{"thought":"推进状态","action":"update_ticket","action_input":"{\"status\":\"in_progress\"}"}`, actComplete},
			wantOutcome: reactOutcomeCompleted,
			wantRounds:  3,
			wantDone:    1,
			wantReply:   1,
			wantUpdate:  1,
		},
		{
			name:        "模型主动 fail",
			responses:   []string{actFail},
			wantOutcome: reactOutcomeFailed,
			wantRounds:  1,
			wantFail:    1,
		},
		{
			name:        "LLM 调用报错即失败，不重试",
			responses:   []string{actComplete},
			errAt:       1,
			wantOutcome: reactOutcomeFailed,
			wantRounds:  1,
			wantFail:    1,
		},
		{
			name:        "轮数耗尽自动完成（保持上游语义）",
			responses:   []string{actSearch},
			maxRounds:   3,
			wantOutcome: reactOutcomeExhausted,
			wantRounds:  3,
			wantDone:    1,
			wantSearch:  []string{"619 错误", "more", "more"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testReactConfig()
			if tt.maxRounds > 0 {
				cfg.MaxRounds = tt.maxRounds
			}
			if tt.window > 0 {
				cfg.HistoryWindow = tt.window
			}
			tools := &fakeReactToolset{responses: tt.responses, errAt: tt.errAt, kbReply: tt.kbReply}

			got := runReActLoop(context.Background(), cfg, tools)

			if got != tt.wantOutcome {
				t.Fatalf("outcome = %q, want %q", got, tt.wantOutcome)
			}
			if len(tools.requests) != tt.wantRounds {
				t.Errorf("chat 调用 %d 轮, want %d", len(tools.requests), tt.wantRounds)
			}
			if len(tools.completed) != tt.wantDone {
				t.Errorf("CompleteStep 调用 %d 次, want %d（%v）", len(tools.completed), tt.wantDone, tools.completed)
			}
			if len(tools.failed) != tt.wantFail {
				t.Errorf("FailStep 调用 %d 次, want %d（%v）", len(tools.failed), tt.wantFail, tools.failed)
			}
			if tt.wantSearch != nil && strings.Join(tools.searches, "|") != strings.Join(tt.wantSearch, "|") {
				t.Errorf("SearchKB 入参 = %v, want %v", tools.searches, tt.wantSearch)
			}
			if len(tools.replies) != tt.wantReply {
				t.Errorf("ReplyUser 调用 %d 次, want %d", len(tools.replies), tt.wantReply)
			}
			if len(tools.updates) != tt.wantUpdate {
				t.Errorf("UpdateTicket 调用 %d 次, want %d", len(tools.updates), tt.wantUpdate)
			}

			last := tools.lastUserMessage()
			for _, want := range tt.lastPrompt {
				if !strings.Contains(last, want) {
					t.Errorf("最后一轮 prompt 缺少 %q\n---\n%s", want, last)
				}
			}
			for _, banned := range tt.notInPrompt {
				if strings.Contains(last, banned) {
					t.Errorf("最后一轮 prompt 不该包含 %q\n---\n%s", banned, last)
				}
			}

			// 每一轮都必须带上系统提示词与基础状态，否则模型无从判断。
			if len(tools.requests) > 0 {
				msgs := tools.requests[0].Messages
				if len(msgs) != 2 || msgs[0].Role != "system" || !strings.Contains(msgs[0].Content, "Relevant knowledge") {
					t.Fatalf("首轮请求消息结构不对: %+v", msgs)
				}
				if msgs[0].Content == "" || !strings.Contains(msgs[0].Content, cfg.TicketTitle) {
					t.Errorf("系统提示词里没有工单标题: %q", msgs[0].Content)
				}
				if tools.requests[0].Model != cfg.Model {
					t.Errorf("请求 model = %q, want %q", tools.requests[0].Model, cfg.Model)
				}
			}
		})
	}
}

func TestRunReActLoopHistoryWindow(t *testing.T) {
	cfg := testReactConfig()
	cfg.HistoryWindow = 2
	cfg.MaxRounds = 10

	responses := make([]string, 0, 8)
	for i := 0; i < 7; i++ {
		responses = append(responses, actSearch)
	}
	responses = append(responses, actComplete)
	tools := &fakeReactToolset{responses: responses}

	if got := runReActLoop(context.Background(), cfg, tools); got != reactOutcomeCompleted {
		t.Fatalf("outcome = %q, want completed", got)
	}

	last := tools.lastUserMessage()
	if !strings.Contains(last, "Round 7:") || !strings.Contains(last, "Round 6:") {
		t.Errorf("窗口应保留最近两轮\n%s", last)
	}
	if strings.Contains(last, "Round 5:") {
		t.Errorf("窗口外的历史不该回灌\n%s", last)
	}
	if len(tools.requests) != 8 {
		t.Errorf("chat 调用 %d 轮, want 8", len(tools.requests))
	}
}

func TestParseReActAction(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantAction string
		wantInput  string
		wantErr    string
	}{
		{
			name:       "标准 JSON",
			raw:        `{"thought":"t","action":"search_kb","action_input":"vpn"}`,
			wantAction: "search_kb",
			wantInput:  "vpn",
		},
		{
			name:       "```json 围栏",
			raw:        "```json\n{\"thought\":\"t\",\"action\":\"complete\",\"action_input\":\"ok\"}\n```",
			wantAction: "complete",
			wantInput:  "ok",
		},
		{
			name:       "无语言标记的围栏",
			raw:        "```\n{\"action\":\"fail\",\"action_input\":\"no access\"}\n```",
			wantAction: "fail",
			wantInput:  "no access",
		},
		{
			name:       "前置解释文字",
			raw:        "Sure! Here is my action:\n{\"action\":\"complete\",\"action_input\":\"done\"}",
			wantAction: "complete",
			wantInput:  "done",
		},
		{
			name:       "action_input 内含 JSON 花括号",
			raw:        `{"action":"update_ticket","action_input":"{\"status\":\"resolved\"}"}`,
			wantAction: "update_ticket",
			wantInput:  `{"status":"resolved"}`,
		},
		{
			name:       "action 前后空格自动裁剪",
			raw:        `{"action":"  complete  ","action_input":"ok"}`,
			wantAction: "complete",
			wantInput:  "ok",
		},
		{
			name:    "空响应",
			raw:     "   ",
			wantErr: "empty response",
		},
		{
			name:    "没有 JSON 对象",
			raw:     "我先想想",
			wantErr: "no JSON object found",
		},
		{
			name:    "JSON 语法错误",
			raw:     `{"action":"complete",,}`,
			wantErr: "invalid JSON",
		},
		{
			name:    "缺 action 字段",
			raw:     `{"thought":"只有想法"}`,
			wantErr: "missing action field",
		},
		{
			name:    "JSON 不完整",
			raw:     `{"action":"complete","action_input":"ok"`,
			wantErr: "no JSON object found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseReActAction(tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want 包含 %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.Action != tt.wantAction {
				t.Errorf("action = %q, want %q", got.Action, tt.wantAction)
			}
			if got.ActionInput != tt.wantInput {
				t.Errorf("action_input = %q, want %q", got.ActionInput, tt.wantInput)
			}
		})
	}
}

func TestBuildReActHistory(t *testing.T) {
	history := []reactRound{
		{Action: ReActAction{Thought: "t1", Action: "search_kb", ActionInput: "a1"}, Result: "r1"},
		{Action: ReActAction{Thought: "t2", Action: "reply_user", ActionInput: "a2"}, Result: "r2"},
		{Action: ReActAction{Thought: "t3", Action: "complete", ActionInput: "a3"}, Result: "r3"},
	}

	full := buildReActHistory(history, 0)
	for _, want := range []string{"Round 1:", "Round 2:", "Round 3:", "action=reply_user", "-> r3"} {
		if !strings.Contains(full, want) {
			t.Errorf("完整历史缺少 %q\n%s", want, full)
		}
	}

	trimmed := buildReActHistory(history, 1)
	if strings.Contains(trimmed, "Round 1:") || strings.Contains(trimmed, "Round 2:") {
		t.Errorf("window=1 只该保留最后一轮\n%s", trimmed)
	}
	if !strings.Contains(trimmed, "Round 3:") {
		t.Errorf("window=1 丢了最后一轮\n%s", trimmed)
	}

	if got := buildReActHistory(nil, 3); got != "" {
		t.Errorf("空历史应为空串, got %q", got)
	}
}

func TestTruncateForEcho(t *testing.T) {
	short := "  hello  "
	if got := truncateForEcho(short); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}

	long := strings.Repeat("x", maxReActRawEchoLength+50)
	got := truncateForEcho(long)
	if len(got) >= len(long) {
		t.Errorf("超长输出应被截断, got len=%d", len(got))
	}
	if !strings.HasSuffix(got, "...(truncated)") {
		t.Errorf("截断后应带标记, got %q", got[len(got)-20:])
	}
}

func TestFormatQueuedUserMessages(t *testing.T) {
	if got := formatQueuedUserMessages(nil); got != "" {
		t.Errorf("空消息应为空串, got %q", got)
	}

	got := formatQueuedUserMessages([]string{"第一条", "第二条", "第三条"})
	want := "\n[User]: 第一条\n[User]: 第二条\n[User]: 第三条"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// 回归：上游只保留 messages[0]，其余用户消息被静默丢弃。
	if strings.Count(got, "[User]") != 3 {
		t.Errorf("三条消息应全部保留, got %q", got)
	}
}

func TestAgentCapabilityScore(t *testing.T) {
	tests := []struct {
		model     string
		maxTokens int
		want      int
	}{
		{"gpt-4o", 0, 100},
		{"GPT-4-Turbo", 0, 100},
		{"claude-3-5-sonnet", 0, 90},
		{"claude-2", 0, 70},
		{"gemini-pro", 0, 80},
		{"gemini-1.5-flash", 0, 60},
		{"qwen2.5-72b", 0, 50},
		{"ernie-4", 0, 40},
		{"some-local-model", 0, 30},
		{"some-local-model", 50000, 35}, // maxTokens/10000 的加成
	}

	for _, tt := range tests {
		if got := agentCapabilityScore(tt.model, tt.maxTokens); got != tt.want {
			t.Errorf("agentCapabilityScore(%q, %d) = %d, want %d", tt.model, tt.maxTokens, got, tt.want)
		}
	}
}
