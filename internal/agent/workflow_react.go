package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/husky/husky/internal/model"
	"github.com/husky/husky/pkg/llm"
)

type ticketUpdate struct {
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

// reActLoop 是 ReAct 循环的适配层：把 DB/LLM 依赖组装成 reactLoopConfig + reactToolset，
// 真正的编排逻辑在 react_loop.go 的 runReActLoop 里（可脱离 DB 与真实模型测试）。
func (s *WorkflowService) reActLoop(ctx context.Context, step *model.WorkflowStep, ticket *model.Ticket, wf *model.Workflow) {
	if s.chatSvc == nil {
		s.failStep(ctx, step, "LLM not configured")
		return
	}

	agent := s.selectAgent(ctx, step)
	if agent == nil {
		s.failStep(ctx, step, "no available agent")
		return
	}

	goal := reactGoal(step)
	cfg := reactLoopConfig{
		StepName:       step.Name,
		Goal:           goal,
		TicketTitle:    ticket.Title,
		TicketStatus:   ticket.Status,
		TicketPriority: ticket.Priority,
		Description:    ticket.Description,
		KBContext:      s.buildKBContext(ctx, ticket, goal),
		Model:          agent.Model,
		Temperature:    agent.Temperature,
		MaxTokens:      agent.MaxTokens,
		MaxRounds:      defaultReActMaxRounds,
		HistoryWindow:  defaultReActHistoryWindow,
	}

	runReActLoop(ctx, cfg, &workflowReactTools{svc: s, step: step, ticket: ticket, agent: agent})
}

// reactGoal 取步骤配置里的 goal，缺省回落到步骤名。
func reactGoal(step *model.WorkflowStep) string {
	var stepCfg struct {
		Goal string `json:"goal"`
	}
	if step.Config != "" {
		if err := json.Unmarshal([]byte(step.Config), &stepCfg); err != nil {
			log.Printf("reActLoop: failed to parse step config: %v", err)
		}
	}
	if stepCfg.Goal != "" {
		return stepCfg.Goal
	}
	return step.Name
}

// buildKBContext 按「工单标题 + 描述 + 目标」检索知识库，拼成提示词里的知识片段。
func (s *WorkflowService) buildKBContext(ctx context.Context, ticket *model.Ticket, goal string) string {
	kbResults, _ := s.kbRepo.SearchSimilar(ctx, model.KnowledgeQuery{
		Query: fmt.Sprintf("%s %s %s", ticket.Title, ticket.Description, goal),
		Limit: 4,
	}, nil)
	var kbContext string
	for _, r := range kbResults {
		kbContext += fmt.Sprintf("- %s: %s\n", r.Title, r.Content)
	}
	return kbContext
}

// workflowReactTools 把 ReAct 循环的副作用接回真实的仓储与 LLM。
type workflowReactTools struct {
	svc    *WorkflowService
	step   *model.WorkflowStep
	ticket *model.Ticket
	agent  *model.Agent
}

func (t *workflowReactTools) Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	return t.svc.chatSvc.Chat(ctx, req)
}

func (t *workflowReactTools) SearchKB(ctx context.Context, query string) string {
	return t.svc.executeSearchKB(ctx, query)
}

func (t *workflowReactTools) ReplyUser(ctx context.Context, msg string) {
	t.svc.executeReplyUser(ctx, t.ticket, t.agent, msg)
}

func (t *workflowReactTools) UpdateTicket(ctx context.Context, input string) string {
	return t.svc.executeUpdateTicket(ctx, t.ticket, input)
}

func (t *workflowReactTools) CompleteStep(ctx context.Context, summary string) {
	t.svc.ticketRepo.AddComment(ctx, &model.Comment{
		TicketID:   t.ticket.ID,
		UserID:     t.svc.agentUserID,
		Content:    fmt.Sprintf("[Agent: %s] Step completed: %s\nResult: %s", t.agent.Name, t.step.Name, summary),
		IsInternal: true,
	})
	t.svc.completeStep(ctx, t.step, summary)
}

func (t *workflowReactTools) FailStep(ctx context.Context, reason string) {
	t.svc.failStep(ctx, t.step, reason)
}

func (s *WorkflowService) executeSearchKB(ctx context.Context, query string) string {
	results, _ := s.kbRepo.SearchSimilar(ctx, model.KnowledgeQuery{Query: query, Limit: 3}, nil)
	var obs []string
	for _, r := range results {
		obs = append(obs, fmt.Sprintf("%s: %s", r.Title, r.Content))
	}
	if len(obs) > 0 {
		return "Observation:\n" + strings.Join(obs, "\n")
	}
	return "Observation: No relevant knowledge found."
}

func (s *WorkflowService) executeReplyUser(ctx context.Context, ticket *model.Ticket, agent *model.Agent, msg string) {
	s.ticketRepo.AddComment(ctx, &model.Comment{
		TicketID:   ticket.ID,
		UserID:     s.agentUserID,
		Content:    fmt.Sprintf("[Agent: %s]\n%s", agent.Name, msg),
		IsInternal: false,
	})
}

func (s *WorkflowService) executeUpdateTicket(ctx context.Context, ticket *model.Ticket, input string) string {
	var update ticketUpdate
	if err := json.Unmarshal([]byte(input), &update); err != nil {
		log.Printf("executeUpdateTicket: failed to parse input: %v", err)
		return "no changes"
	}
	var changes []string
	if update.Status != "" && model.IsValidTransition(model.TicketStatus(ticket.Status), model.TicketStatus(update.Status)) {
		ticket.Status = update.Status
		if err := s.ticketRepo.Update(ctx, ticket); err != nil {
			log.Printf("executeUpdateTicket: failed to update status: %v", err)
			return "update conflict, retrying"
		}
		changes = append(changes, "status="+update.Status)
	}
	if update.Priority != "" && model.IsValidPriority(update.Priority) {
		ticket.Priority = update.Priority
		if err := s.ticketRepo.Update(ctx, ticket); err != nil {
			log.Printf("executeUpdateTicket: failed to update priority: %v", err)
			return "update conflict, retrying"
		}
		changes = append(changes, "priority="+update.Priority)
	}
	if len(changes) > 0 {
		return "updated: " + strings.Join(changes, ", ")
	}
	return "no changes"
}

func (s *WorkflowService) selectAgent(ctx context.Context, step *model.WorkflowStep) *model.Agent {
	if step.AgentID != nil {
		agent, err := s.agentRepo.GetByID(ctx, *step.AgentID)
		if err == nil && agent.Enabled {
			return agent
		}
	}
	agents, _, err := s.agentRepo.List(ctx, 0, 100, "")
	if err != nil {
		log.Printf("findAvailableAgent: failed to list agents: %v", err)
		return nil
	}
	var best *model.Agent
	var bestScore int
	for i := range agents {
		a := &agents[i]
		if a.Enabled && (a.Type == "llm" || a.Type == "hybrid") {
			score := agentCapabilityScore(a.Model, a.MaxTokens)
			if best == nil || score > bestScore {
				best = a
				bestScore = score
			}
		}
	}
	return best
}

// agentCapabilityScore assigns a priority score to an agent model.
// Higher scores indicate more capable models.
func agentCapabilityScore(model string, maxTokens int) int {
	score := 0
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "gpt-4") || strings.Contains(m, "gpt4"):
		score = 100
	case strings.Contains(m, "gpt-3.5") || strings.Contains(m, "gpt3.5"):
		score = 50
	case strings.Contains(m, "claude-3"):
		score = 90
	case strings.Contains(m, "claude"):
		score = 70
	case strings.Contains(m, "gemini-pro"):
		score = 80
	case strings.Contains(m, "gemini"):
		score = 60
	case strings.Contains(m, "qwen") || strings.Contains(m, "通义"):
		score = 50
	case strings.Contains(m, "baidu") || strings.Contains(m, "ernie"):
		score = 40
	default:
		score = 30
	}
	if maxTokens > 0 {
		score += maxTokens / 10000
	}
	return score
}

func (s *WorkflowService) reActLoopWithQueue(ctx context.Context, step *model.WorkflowStep, ticket *model.Ticket, wf *model.Workflow, msgCh chan string) {
	var messages []string
	for {
		select {
		case msg, ok := <-msgCh:
			if !ok {
				return
			}
			messages = append(messages, msg)
			if len(messages) >= 16 {
				step.Result += formatQueuedUserMessages(messages)
				s.wfRepo.UpdateStep(ctx, step)
				s.reActLoop(ctx, step, ticket, wf)
				messages = nil
			}
		default:
			if len(messages) > 0 {
				step.Result += formatQueuedUserMessages(messages)
				s.wfRepo.UpdateStep(ctx, step)
				s.reActLoop(ctx, step, ticket, wf)
				messages = nil
			}
			return
		}
	}
}
