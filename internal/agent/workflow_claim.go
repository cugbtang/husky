package agent

import (
	"context"
	"log"
	"sort"
	"time"

	"github.com/husky/husky/internal/model"
	"github.com/husky/husky/internal/repository"
	"github.com/husky/husky/pkg/pglock"
	"gorm.io/gorm"
)

// 未抢到步骤时的原因，写进日志也写进测试断言。
const (
	claimReasonClaimed     = "claimed"
	claimReasonExhausted   = "no_pending_step"
	claimReasonStepRunning = "another_step_running"
	claimReasonLostRace    = "claim_lost_race"
	claimReasonNotRunning  = "workflow_not_running"
	claimReasonMissing     = "workflow_missing"
	claimReasonLocked      = "locked_by_another_replica"
)

// stepClaimRepo 是「抢占下一步」所需的仓储能力。
//
// 抽成接口的理由和 reActLoop 的 reactToolset 一致：把接口开在副作用边界上，
// 推进逻辑就能在没有数据库的情况下被测试——上游这段逻辑零覆盖，恰恰是因为
// 它直接依赖具体仓储。
type stepClaimRepo interface {
	GetByID(ctx context.Context, id uint) (*model.Workflow, error)
	GetPendingSteps(ctx context.Context, workflowID uint) ([]model.WorkflowStep, error)
	HasRunningStep(ctx context.Context, workflowID uint) (bool, error)
	ClaimStep(ctx context.Context, stepID uint, startedAt time.Time) (bool, error)
	Update(ctx context.Context, wf *model.Workflow) error
}

// 编译期确认真实仓储满足接口：签名一旦对不上，这里先炸，而不是等到运行期 nil panic。
var _ stepClaimRepo = (*repository.WorkflowRepository)(nil)

// claimResult 描述一次「能不能往下推」的判定结果。
//
// 三选一：Step 非空 = 抢到了这一步；Exhausted = 没有待执行步骤了，工作流可以收尾；
// 都为假 = 本次什么都不做（Reason 说明原因）。
type claimResult struct {
	WF        *model.Workflow
	Step      *model.WorkflowStep
	Exhausted bool
	Reason    string
}

// claimNextStep 判定「这个 workflow 现在该执行哪一步」，只读不写。
//
// 写操作留给调用方在持锁状态下做，这样「读判定 → 写状态」是一段临界区，
// 单测里可以只验判定、不碰数据库。
func claimNextStep(ctx context.Context, repo stepClaimRepo, workflowID uint) (claimResult, error) {
	wf, err := repo.GetByID(ctx, workflowID)
	if err != nil {
		return claimResult{}, err
	}
	if wf == nil {
		return claimResult{Reason: claimReasonMissing}, nil
	}
	// 已失败 / 已完成的工作流不该再往下走。上游没有这个判断：一条已经 failed 的
	// 工作流只要有人再调一次推进，后面所有步骤会照跑不误。
	if wf.Status != "running" {
		return claimResult{WF: wf, Reason: claimReasonNotRunning}, nil
	}

	running, err := repo.HasRunningStep(ctx, workflowID)
	if err != nil {
		return claimResult{}, err
	}
	if running {
		// 已经有步骤在跑了。可能是另一个副本刚抢到，也可能是人工步骤还在等人处理。
		// 无论哪种，现在都不该启动后一步——否则两步并发、顺序错乱。
		return claimResult{WF: wf, Reason: claimReasonStepRunning}, nil
	}

	pending, err := repo.GetPendingSteps(ctx, workflowID)
	if err != nil {
		return claimResult{}, err
	}
	if len(pending) == 0 {
		return claimResult{WF: wf, Exhausted: true, Reason: claimReasonExhausted}, nil
	}

	// 仓储已经按 step_index 排序，这里再排一次：判定逻辑不该依赖调用方顺序。
	sort.SliceStable(pending, func(i, j int) bool {
		return pending[i].StepIndex < pending[j].StepIndex
	})
	step := pending[0]

	claimed, err := repo.ClaimStep(ctx, step.ID, time.Now())
	if err != nil {
		return claimResult{}, err
	}
	if !claimed {
		// CAS 失败：别的执行者刚好抢先把它置成 running 了。
		return claimResult{WF: wf, Reason: claimReasonLostRace}, nil
	}

	now := time.Now()
	step.Status = "running"
	step.StartedAt = &now
	return claimResult{WF: wf, Step: &step, Reason: claimReasonClaimed}, nil
}

// applyClaim 把判定结果落库（工作流指针前移 / 工作流收尾）。
func applyClaim(ctx context.Context, repo stepClaimRepo, res *claimResult) error {
	switch {
	case res.Exhausted:
		res.WF.Status = "completed"
		return repo.Update(ctx, res.WF)
	case res.Step != nil:
		res.WF.CurrentStep = res.Step.StepIndex
		return repo.Update(ctx, res.WF)
	default:
		return nil
	}
}

// workflowLocker 是推进锁的最小接口，默认实现是 PostgreSQL advisory lock。
//
// 单副本部署、本地 sqlite 和单测里不需要它：locker 为 nil 时直接放行。
type workflowLocker interface {
	Acquire(ctx context.Context, workflowID uint) (release func(), acquired bool, err error)
}

type pgWorkflowLocker struct {
	db *gorm.DB
}

func (l pgWorkflowLocker) Acquire(ctx context.Context, workflowID uint) (func(), bool, error) {
	return pglock.Acquire(ctx, l.db, pglock.WorkflowKey(workflowID))
}

// SetDB 打开跨副本互斥。不调用（或传 nil）时退化为单进程行为。
func (s *WorkflowService) SetDB(db *gorm.DB) {
	if db == nil {
		s.locker = nil
		return
	}
	s.locker = pgWorkflowLocker{db: db}
}

// workflowLock 取推进锁。返回的 acquired=false 表示另一个副本正在推进同一个
// workflow，本次应当直接放弃（它会一路推下去）。
//
// 注意：advisory lock 会占住一条专用连接直到解锁，锁内还要发查询，所以连接池
// 不能配成 1（默认 100），否则这里会自己等自己。
func (s *WorkflowService) workflowLock(ctx context.Context, workflowID uint) (release func(), acquired bool) {
	noop := func() {}
	if s.locker == nil {
		return noop, true
	}
	release, acquired, err := s.locker.Acquire(ctx, workflowID)
	if err != nil {
		// 锁本身出错（连接池耗尽、DB 抖动）时不阻断推进：ClaimStep 的 CAS 仍然
		// 保证同一步只被执行一次，丢掉的只是「不重复启动后一步」这层保护。
		log.Printf("executeNextStep: acquire lock for workflow %d failed: %v (continuing without lock)", workflowID, err)
		return noop, true
	}
	if !acquired {
		return noop, false
	}
	if release == nil {
		return noop, true
	}
	return release, true
}

// claimNextForWorkflow 在锁内完成「判定 + 落库」，出锁后才由调用方执行步骤。
func (s *WorkflowService) claimNextForWorkflow(ctx context.Context, repo stepClaimRepo, workflowID uint) (claimResult, error) {
	release, acquired := s.workflowLock(ctx, workflowID)
	if !acquired {
		return claimResult{Reason: claimReasonLocked}, nil
	}

	var res claimResult
	var err error
	func() {
		defer release()
		res, err = claimNextStep(ctx, repo, workflowID)
		if err != nil || (res.Step == nil && !res.Exhausted) {
			return
		}
		if applyErr := applyClaim(ctx, repo, &res); applyErr != nil {
			// 步骤已经抢到手了，工作流指针写失败只是展示信息不准，
			// 不该因此放弃执行——否则这一步会永远停在 running。
			log.Printf("executeNextStep: workflow %d: persist workflow state failed: %v", workflowID, applyErr)
		}
	}()
	return res, err
}
