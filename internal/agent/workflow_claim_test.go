package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/husky/husky/internal/model"
)

// ---------- 构造辅助 ----------
//
// ID 来自内嵌的 model.Base，结构体字面量里没法直接赋值，统一走这两个构造函数。

func newWorkflow(id uint, status string, total int) *model.Workflow {
	wf := &model.Workflow{TicketID: 3, Status: status, TotalSteps: total}
	wf.ID = id
	return wf
}

func newStep(id uint, index int) model.WorkflowStep {
	s := model.WorkflowStep{StepIndex: index, Status: "pending"}
	s.ID = id
	return s
}

func ptrStep(id uint, index int) *model.WorkflowStep {
	s := newStep(id, index)
	return &s
}

// ---------- 假仓储 ----------

// fakeClaimRepo 记录调用轨迹，并允许逐项注入错误/返回值。
type fakeClaimRepo struct {
	wf            *model.Workflow
	wfErr         error
	pending       []model.WorkflowStep
	pendingErr    error
	hasRunning    bool
	hasRunningErr error
	claimOK       bool
	claimErr      error

	calls      []string
	claimedIDs []uint
	updates    []model.Workflow
}

func (f *fakeClaimRepo) GetByID(_ context.Context, _ uint) (*model.Workflow, error) {
	f.calls = append(f.calls, "GetByID")
	if f.wfErr != nil {
		return nil, f.wfErr
	}
	return f.wf, nil
}

func (f *fakeClaimRepo) GetPendingSteps(_ context.Context, _ uint) ([]model.WorkflowStep, error) {
	f.calls = append(f.calls, "GetPendingSteps")
	if f.pendingErr != nil {
		return nil, f.pendingErr
	}
	return f.pending, nil
}

func (f *fakeClaimRepo) HasRunningStep(_ context.Context, _ uint) (bool, error) {
	f.calls = append(f.calls, "HasRunningStep")
	return f.hasRunning, f.hasRunningErr
}

func (f *fakeClaimRepo) ClaimStep(_ context.Context, stepID uint, _ time.Time) (bool, error) {
	f.calls = append(f.calls, "ClaimStep")
	f.claimedIDs = append(f.claimedIDs, stepID)
	return f.claimOK, f.claimErr
}

func (f *fakeClaimRepo) Update(_ context.Context, wf *model.Workflow) error {
	f.calls = append(f.calls, "Update")
	f.updates = append(f.updates, *wf)
	return nil
}

func (f *fakeClaimRepo) called(method string) bool {
	for _, c := range f.calls {
		if c == method {
			return true
		}
	}
	return false
}

// fakeLocker 模拟 advisory lock，并记录是否真的持有。
type fakeLocker struct {
	mu       sync.Mutex
	grant    bool
	err      error
	held     bool
	acquires int
	releases int
}

func (l *fakeLocker) Acquire(_ context.Context, _ uint) (func(), bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.acquires++
	if l.err != nil {
		return nil, false, l.err
	}
	if !l.grant {
		return func() {}, false, nil
	}
	l.held = true
	return func() {
		l.mu.Lock()
		l.held = false
		l.releases++
		l.mu.Unlock()
	}, true, nil
}

func (l *fakeLocker) isHeld() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

func runningWorkflow() *model.Workflow {
	return newWorkflow(7, "running", 3)
}

// ---------- claimNextStep ----------

func TestClaimNextStep(t *testing.T) {
	tests := []struct {
		name        string
		repo        *fakeClaimRepo
		wantReason  string
		wantExhaust bool
		wantStepID  uint
		wantIndex   int
		wantErr     bool
		wantNoClaim bool // 不该调用 ClaimStep
	}{
		{
			name:       "抢到最小 index 的待执行步骤",
			repo:       &fakeClaimRepo{wf: runningWorkflow(), pending: []model.WorkflowStep{newStep(11, 0), newStep(12, 1)}, claimOK: true},
			wantReason: claimReasonClaimed,
			wantStepID: 11,
			wantIndex:  0,
		},
		{
			name:       "pending 乱序也选 index 最小的",
			repo:       &fakeClaimRepo{wf: runningWorkflow(), pending: []model.WorkflowStep{newStep(22, 2), newStep(21, 1)}, claimOK: true},
			wantReason: claimReasonClaimed,
			wantStepID: 21,
			wantIndex:  1,
		},
		{
			name:        "没有待执行步骤 -> 工作流收尾",
			repo:        &fakeClaimRepo{wf: runningWorkflow()},
			wantReason:  claimReasonExhausted,
			wantExhaust: true,
			wantNoClaim: true,
		},
		{
			name:        "已有步骤在跑 -> 不启动后一步",
			repo:        &fakeClaimRepo{wf: runningWorkflow(), hasRunning: true, pending: []model.WorkflowStep{newStep(11, 0)}, claimOK: true},
			wantReason:  claimReasonStepRunning,
			wantNoClaim: true,
		},
		{
			name:        "工作流已失败 -> 不推进",
			repo:        &fakeClaimRepo{wf: newWorkflow(7, "failed", 3), pending: []model.WorkflowStep{newStep(11, 0)}, claimOK: true},
			wantReason:  claimReasonNotRunning,
			wantNoClaim: true,
		},
		{
			name:        "工作流已完成 -> 不推进",
			repo:        &fakeClaimRepo{wf: newWorkflow(7, "completed", 3), pending: []model.WorkflowStep{newStep(11, 0)}, claimOK: true},
			wantReason:  claimReasonNotRunning,
			wantNoClaim: true,
		},
		{
			name:        "工作流状态为空串 -> 不推进",
			repo:        &fakeClaimRepo{wf: newWorkflow(7, "", 0), pending: []model.WorkflowStep{newStep(11, 0)}, claimOK: true},
			wantReason:  claimReasonNotRunning,
			wantNoClaim: true,
		},
		{
			name:        "CAS 抢占失败 -> 让出",
			repo:        &fakeClaimRepo{wf: runningWorkflow(), pending: []model.WorkflowStep{newStep(11, 0)}, claimOK: false},
			wantReason:  claimReasonLostRace,
			wantNoClaim: false,
		},
		{
			name:       "工作流不存在",
			repo:       &fakeClaimRepo{},
			wantReason: claimReasonMissing,
		},
		{
			name:    "读工作流报错",
			repo:    &fakeClaimRepo{wfErr: errors.New("db down")},
			wantErr: true,
		},
		{
			name:    "查待执行步骤报错",
			repo:    &fakeClaimRepo{wf: runningWorkflow(), pendingErr: errors.New("db down")},
			wantErr: true,
		},
		{
			name:    "查 running 步骤报错",
			repo:    &fakeClaimRepo{wf: runningWorkflow(), hasRunningErr: errors.New("db down")},
			wantErr: true,
		},
		{
			name:    "CAS 报错",
			repo:    &fakeClaimRepo{wf: runningWorkflow(), pending: []model.WorkflowStep{newStep(11, 0)}, claimErr: errors.New("db down")},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := claimNextStep(context.Background(), tt.repo, 7)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际 err=nil, res=%+v", res)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外报错: %v", err)
			}
			if res.Reason != tt.wantReason {
				t.Fatalf("reason=%q, 期望 %q", res.Reason, tt.wantReason)
			}
			if res.Exhausted != tt.wantExhaust {
				t.Fatalf("exhausted=%v, 期望 %v", res.Exhausted, tt.wantExhaust)
			}
			if tt.wantStepID == 0 {
				if res.Step != nil {
					t.Fatalf("不该抢到步骤，实际 %+v", res.Step)
				}
			} else {
				if res.Step == nil {
					t.Fatalf("期望抢到步骤 %d，实际 nil", tt.wantStepID)
				}
				if res.Step.ID != tt.wantStepID {
					t.Fatalf("抢到步骤 %d, 期望 %d", res.Step.ID, tt.wantStepID)
				}
				if res.Step.StepIndex != tt.wantIndex {
					t.Fatalf("step_index=%d, 期望 %d", res.Step.StepIndex, tt.wantIndex)
				}
				if res.Step.Status != "running" {
					t.Fatalf("抢到后状态应为 running，实际 %q", res.Step.Status)
				}
				if res.Step.StartedAt == nil {
					t.Fatal("抢到后 should 设置 started_at")
				}
			}
			if tt.wantNoClaim && tt.repo.called("ClaimStep") {
				t.Fatal("这条路径不该调用 ClaimStep")
			}
		})
	}
}

// 关键性质：即使 advisory lock 完全失效（多副本各自以为自己独占），
// 数据库级 CAS 也必须保证**同一步只被一个执行者抢到**。
//
// 注意这里断言的是「同一步不重复」，而不是「只有一个执行者抢到」：假仓储没有锁，
// HasRunningStep 与 ClaimStep 之间天然有窗口，所以确实可能同时启动两个不同的步骤。
// 挡住这件事的是第一层（advisory lock），CAS 是第二层兜底——两层各管一段，
// 这个测试只验第二层，避免把它写成「有了 CAS 就不需要锁」的错误结论。
func TestClaimNextStepCASWinsUnderConcurrency(t *testing.T) {
	repo := newCASClaimRepo(
		newWorkflow(7, "running", 2),
		[]model.WorkflowStep{newStep(11, 0), newStep(12, 1)},
	)

	const goroutines = 50
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed = map[uint]int{}
	)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := claimNextStep(context.Background(), repo, 7)
			if err != nil {
				return
			}
			if res.Step != nil {
				mu.Lock()
				claimed[res.Step.ID]++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(claimed) == 0 {
		t.Fatal("50 个并发执行者没有一个抢到步骤")
	}
	for stepID, n := range claimed {
		if n != 1 {
			t.Fatalf("步骤 %d 被 %d 个执行者同时抢到——CAS 失效", stepID, n)
		}
	}
	if running := repo.runningCount(); running != len(claimed) {
		t.Fatalf("running 步骤数=%d, 抢到的步骤数=%d, 两者应一致", running, len(claimed))
	}
}

// casClaimRepo 用内存模拟「带 status 条件的 UPDATE」的原子语义。
type casClaimRepo struct {
	mu    sync.Mutex
	wf    *model.Workflow
	steps []model.WorkflowStep
}

func newCASClaimRepo(wf *model.Workflow, steps []model.WorkflowStep) *casClaimRepo {
	return &casClaimRepo{wf: wf, steps: steps}
}

func (r *casClaimRepo) GetByID(_ context.Context, _ uint) (*model.Workflow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *r.wf
	return &cp, nil
}

func (r *casClaimRepo) GetPendingSteps(_ context.Context, _ uint) ([]model.WorkflowStep, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []model.WorkflowStep
	for _, s := range r.steps {
		if s.Status == "pending" {
			out = append(out, s)
		}
	}
	return out, nil
}

func (r *casClaimRepo) HasRunningStep(_ context.Context, _ uint) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runningCountLocked() > 0, nil
}

func (r *casClaimRepo) ClaimStep(_ context.Context, stepID uint, at time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.steps {
		if r.steps[i].ID != stepID {
			continue
		}
		if r.steps[i].Status != "pending" {
			return false, nil
		}
		r.steps[i].Status = "running"
		r.steps[i].StartedAt = &at
		return true, nil
	}
	return false, nil
}

func (r *casClaimRepo) Update(_ context.Context, _ *model.Workflow) error { return nil }

func (r *casClaimRepo) runningCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runningCountLocked()
}

func (r *casClaimRepo) runningCountLocked() int {
	n := 0
	for _, s := range r.steps {
		if s.Status == "running" {
			n++
		}
	}
	return n
}

// ---------- applyClaim ----------

func TestApplyClaim(t *testing.T) {
	t.Run("收尾时把工作流置为 completed", func(t *testing.T) {
		repo := &fakeClaimRepo{}
		wf := runningWorkflow()
		res := claimResult{WF: wf, Exhausted: true}
		if err := applyClaim(context.Background(), repo, &res); err != nil {
			t.Fatalf("意外报错: %v", err)
		}
		if wf.Status != "completed" {
			t.Fatalf("状态=%q, 期望 completed", wf.Status)
		}
		if len(repo.updates) != 1 {
			t.Fatalf("写库次数=%d, 期望 1", len(repo.updates))
		}
	})

	t.Run("抢到步骤时前移 CurrentStep", func(t *testing.T) {
		repo := &fakeClaimRepo{}
		wf := runningWorkflow()
		res := claimResult{WF: wf, Step: ptrStep(12, 2)}
		if err := applyClaim(context.Background(), repo, &res); err != nil {
			t.Fatalf("意外报错: %v", err)
		}
		if wf.CurrentStep != 2 {
			t.Fatalf("CurrentStep=%d, 期望 2", wf.CurrentStep)
		}
	})

	t.Run("什么都没抢到时不写库", func(t *testing.T) {
		repo := &fakeClaimRepo{}
		res := claimResult{WF: runningWorkflow(), Reason: claimReasonStepRunning}
		if err := applyClaim(context.Background(), repo, &res); err != nil {
			t.Fatalf("意外报错: %v", err)
		}
		if len(repo.updates) != 0 || repo.called("Update") {
			t.Fatal("这条路径不该写工作流")
		}
	})
}

// ---------- claimNextForWorkflow：锁的语义 ----------

func TestClaimNextForWorkflowLockSemantics(t *testing.T) {
	newReadyRepo := func() *fakeClaimRepo {
		return &fakeClaimRepo{
			wf:      runningWorkflow(),
			pending: []model.WorkflowStep{newStep(11, 0)},
			claimOK: true,
		}
	}

	t.Run("拿不到锁就完全不碰数据库", func(t *testing.T) {
		repo := newReadyRepo()
		s := &WorkflowService{locker: &fakeLocker{grant: false}}
		res, err := s.claimNextForWorkflow(context.Background(), repo, 7)
		if err != nil {
			t.Fatalf("意外报错: %v", err)
		}
		if res.Reason != claimReasonLocked {
			t.Fatalf("reason=%q, 期望 %q", res.Reason, claimReasonLocked)
		}
		if len(repo.calls) != 0 {
			t.Fatalf("没拿到锁却调用了仓储: %v", repo.calls)
		}
	})

	t.Run("出锁后才返回，锁不跨越步骤执行", func(t *testing.T) {
		repo := newReadyRepo()
		locker := &fakeLocker{grant: true}
		s := &WorkflowService{locker: locker}
		res, err := s.claimNextForWorkflow(context.Background(), repo, 7)
		if err != nil {
			t.Fatalf("意外报错: %v", err)
		}
		if res.Step == nil {
			t.Fatalf("没抢到步骤, reason=%q", res.Reason)
		}
		if locker.isHeld() {
			t.Fatal("claimNextForWorkflow 返回时锁仍被持有——执行阶段会长占连接")
		}
		if locker.acquires != 1 || locker.releases != 1 {
			t.Fatalf("acquire=%d release=%d, 期望各 1", locker.acquires, locker.releases)
		}
	})

	t.Run("锁服务报错时退化为无锁推进", func(t *testing.T) {
		repo := newReadyRepo()
		s := &WorkflowService{locker: &fakeLocker{err: errors.New("pool exhausted")}}
		res, err := s.claimNextForWorkflow(context.Background(), repo, 7)
		if err != nil {
			t.Fatalf("锁报错不该让推进失败: %v", err)
		}
		if res.Step == nil {
			t.Fatalf("应当仍然抢到步骤, reason=%q", res.Reason)
		}
		if !repo.called("ClaimStep") {
			t.Fatal("应当仍然走 CAS 抢占")
		}
	})

	t.Run("没有 locker（单副本/sqlite）时直接推进", func(t *testing.T) {
		repo := newReadyRepo()
		s := &WorkflowService{}
		res, err := s.claimNextForWorkflow(context.Background(), repo, 7)
		if err != nil {
			t.Fatalf("意外报错: %v", err)
		}
		if res.Step == nil {
			t.Fatalf("应当抢到步骤, reason=%q", res.Reason)
		}
	})

	t.Run("没有待执行步骤时在锁内收尾", func(t *testing.T) {
		repo := &fakeClaimRepo{wf: runningWorkflow()}
		s := &WorkflowService{locker: &fakeLocker{grant: true}}
		res, err := s.claimNextForWorkflow(context.Background(), repo, 7)
		if err != nil {
			t.Fatalf("意外报错: %v", err)
		}
		if !res.Exhausted {
			t.Fatalf("期望 exhausted, reason=%q", res.Reason)
		}
		if repo.wf.Status != "completed" {
			t.Fatalf("工作流状态=%q, 期望 completed", repo.wf.Status)
		}
	})

	t.Run("工作流指针写失败不影响抢到的步骤", func(t *testing.T) {
		repo := &fakeClaimRepo{
			wf:      runningWorkflow(),
			pending: []model.WorkflowStep{newStep(11, 0)},
			claimOK: true,
		}
		// Update 失败：用一个在 Update 上返回错误的仓储（fakeClaimRepo 不支持，
		// 这里换成最简包装）。
		failing := &updateFailingRepo{fakeClaimRepo: repo}
		s := &WorkflowService{locker: &fakeLocker{grant: true}}
		res, err := s.claimNextForWorkflow(context.Background(), failing, 7)
		if err != nil {
			t.Fatalf("工作流指针写失败不该让推进整体失败: %v", err)
		}
		if res.Step == nil {
			t.Fatal("步骤已抢到，不该丢弃")
		}
	})
}

type updateFailingRepo struct{ *fakeClaimRepo }

func (r *updateFailingRepo) Update(_ context.Context, _ *model.Workflow) error {
	return errors.New("write conflict")
}

func TestSetDB(t *testing.T) {
	s := &WorkflowService{}
	s.SetDB(nil)
	if s.locker != nil {
		t.Fatal("传 nil 应退化为无锁模式")
	}
}
