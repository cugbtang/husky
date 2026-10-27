// Command worker 是 Husky 的独立后台任务进程。
//
// 为什么需要它：SLA 扫描原本跑在 API 进程内部（router.SetupRouter 里启动一个 5 分钟
// ticker）。API 一旦扩到多副本，每个副本都会各跑一份扫描器——同一条超时工单会被
// 重复升级、重复发站内通知、重复打 webhook。
//
// worker 把后台任务从 API 进程里搬出来，以单副本方式运行，并用 PostgreSQL
// advisory lock 保证全局只有一个实例在扫描。API 侧通过环境变量
// DISABLE_INPROC_ESCALATOR=1 关闭内嵌扫描器，避免两边同时跑。
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/husky/husky/internal/config"
	"github.com/husky/husky/internal/database"
	"github.com/husky/husky/internal/repository"
	"github.com/husky/husky/internal/ticket"
	"github.com/husky/husky/pkg/logger"
)

const (
	defaultInterval = time.Minute
	// minInterval 兜底限制，避免手抖填 1s 把数据库打满
	minInterval = 10 * time.Second
	// advisoryLockKey 是 worker 进程级互斥锁的 key（ASCII "HUSK"）
	advisoryLockKey int64 = 0x4855534B
)

// job 是一个可被调度执行的后台任务。
// 返回值表示本轮处理的记录数，便于在日志里观察任务是否真的在干活。
type job struct {
	name string
	run  func(ctx context.Context) (int, error)
}

func main() {
	configPath := flag.String("config", "config.yaml", "配置文件路径；文件不存在时自动回退到环境变量")
	interval := flag.Duration("interval", defaultInterval, "SLA 扫描间隔")
	once := flag.Bool("once", false, "只执行一轮扫描后退出（适合 cron 或本地调试）")
	noLock := flag.Bool("no-lock", false, "跳过多副本互斥锁（仅本地调试用）")
	flag.Parse()

	if *interval < minInterval {
		log.Fatalf("interval %v is too small, minimum is %v", *interval, minInterval)
	}

	cfg, err := config.LoadWithFallback(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	logInstance, err := logger.NewLogger(cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := database.NewDatabase(database.Config{
		Host:            cfg.Database.Host,
		Port:            cfg.Database.Port,
		User:            cfg.Database.User,
		Password:        cfg.Database.Password,
		DBName:          cfg.Database.Name,
		SSLMode:         cfg.Database.SSLMode,
		MaxIdleConns:    cfg.Database.MaxIdleConns,
		MaxOpenConns:    cfg.Database.MaxOpenConns,
		ConnMaxLifetime: time.Duration(cfg.Database.ConnMaxLifetime) * time.Second,
	})
	if err != nil {
		logInstance.Fatal("Failed to initialize database", "error", err)
	}
	logInstance.Info("Worker connected to database")

	// worker 不执行 AutoMigrate：schema 归 API 进程所有，
	// 两个进程同时迁移会互相打架。因此 worker 必须在 API 至少启动过一次之后再起。

	if !*noLock {
		release, ok, err := acquireSingletonLock(ctx, db, advisoryLockKey)
		if err != nil {
			logInstance.Fatal("Failed to acquire singleton lock", "error", err)
		}
		if !ok {
			// 已有实例在跑，当前进程直接退出，由编排层重启即可
			logInstance.Warn("Another worker instance holds the lock, exiting")
			return
		}
		defer release()
		logInstance.Info("Singleton lock acquired", "key", advisoryLockKey)
	}

	ticketRepo := repository.NewTicketRepository(db)
	slaConfigRepo := repository.NewSLAConfigRepository(db)
	webhookRepo := repository.NewWebhookConfigRepository(db)
	sysConfigRepo := repository.NewSystemConfigRepository(db)

	// eventHandler 传 nil：worker 只负责站内通知 + 推送方式 + webhook 外发，
	// 渠道消息（飞书等）由 API 侧 gateway 承担，避免 worker 重复拉起渠道客户端。
	// FireSLAEvent 内部对 nil 做了判空，不会 panic。
	slaSvc := ticket.NewSLAConfigService(slaConfigRepo, webhookRepo, sysConfigRepo, nil, ticketRepo)
	escalator := ticket.NewSLAEscalator(ticketRepo, slaSvc)

	jobs := []job{
		{name: "sla-escalation", run: escalator.RunOnce},
	}

	if *once {
		runAll(ctx, jobs, logInstance)
		return
	}

	runLoop(ctx, jobs, *interval, logInstance)
}

// runLoop 启动即执行一轮，之后按 interval 周期执行，直到收到退出信号。
func runLoop(ctx context.Context, jobs []job, interval time.Duration, log *logger.Logger) {
	log.Info("Worker started",
		"interval", interval.String(),
		"jobs", len(jobs),
	)

	runAll(ctx, jobs, log)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("Worker shutting down")
			return
		case <-ticker.C:
			runAll(ctx, jobs, log)
		}
	}
}

// runAll 顺序执行一轮任务。单个任务失败不影响其它任务。
func runAll(ctx context.Context, jobs []job, log *logger.Logger) {
	for _, j := range jobs {
		start := time.Now()
		n, err := j.run(ctx)
		dur := time.Since(start)
		if err != nil {
			log.Error("Job failed", "job", j.name, "error", err, "duration", dur.String())
			continue
		}
		log.Info("Job done", "job", j.name, "changed", n, "duration", dur.String())
	}
}
