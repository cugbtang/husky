// Package pglock 提供基于 PostgreSQL advisory lock 的进程间互斥。
//
// 为什么需要它：这个项目里有两处「只能有一个人干」的工作，而且都会在多副本
// 部署下被破坏：
//
//  1. schema 迁移（internal/database.AutoMigrate）——API 扩到 2 个副本时，
//     两个进程会同时 CREATE TABLE / ALTER TABLE，Postgres 会抛 duplicate key
//     on pg_type 或死锁，然后进程 Fatal 退出，滚动更新直接变成 CrashLoopBackOff。
//  2. SLA 扫描（cmd/worker）——多实例同时扫描，同一条超时工单会被重复升级、
//     重复发通知、重复打 webhook。
//
// 为什么选 advisory lock 而不是状态表 / SELECT FOR UPDATE：
//   - 不落任何 schema，不需要额外的表和清理逻辑；
//   - 进程崩溃、容器被 kill 时，数据库会话结束，锁自动释放，没有「死锁残留」问题；
//   - 不需要事务包裹，不会因为长事务把 vacuum 拖住。
//
// ⚠️ 关键实现细节：advisory lock 是**会话级**的。用连接池的 db.Exec 去加锁，
// 加锁和解锁可能落在不同连接上，锁就变成了别人的。所以加锁必须固定占住一条
// *sql.Conn，直到解锁为止。
package pglock

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// 锁 key。取 ASCII 助记字符串，避免和集群里其他应用撞 key。
// 不同用途必须是不同的 key，否则迁移会挡住 worker、worker 会挡住迁移。
const (
	// WorkerKey 保护后台任务单例（"HUSK"）
	WorkerKey int64 = 0x4855534B
	// MigrationKey 保护 schema 迁移（"HUSM"）
	MigrationKey int64 = 0x4855534D
)

// noop 用于「不需要锁」的路径，保证释放函数永远可调用。
var noop = func() {}

// Acquire 尝试加锁，拿不到就立刻返回 acquired=false。
//
// 适合「拿不到就该退出，由编排层重启」的场景（worker 单例）。
func Acquire(ctx context.Context, db *gorm.DB, key int64) (release func(), acquired bool, err error) {
	conn, ok, err := acquireConn(ctx, db, key, false)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return noop, false, nil
	}
	return func() { unlockConn(conn, key) }, true, nil
}

// AcquireBlocking 加锁，拿不到就阻塞等待，直到拿到或 ctx 超时。
//
// 适合「必须做完，但可以排队」的场景（schema 迁移）：滚动更新时第二个副本
// 会等在第一个副本后面，而不是失败退出。
func AcquireBlocking(ctx context.Context, db *gorm.DB, key int64) (release func(), err error) {
	conn, ok, err := acquireConn(ctx, db, key, true)
	if err != nil {
		return nil, err
	}
	if !ok {
		// 只有 ctx 被取消时才会走到这里
		return noop, ctx.Err()
	}
	return func() { unlockConn(conn, key) }, nil
}

// acquireConn 取一条专用连接并在其上加锁。
//
// 非 PostgreSQL 方言（本地 sqlite 单测等）不支持 advisory lock，直接放行，
// 单例由部署层（replicas: 1）保证。
func acquireConn(ctx context.Context, db *gorm.DB, key int64, blocking bool) (*sql.Conn, bool, error) {
	if db.Dialector.Name() != "postgres" {
		return nil, true, nil
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, false, fmt.Errorf("get sql.DB: %w", err)
	}

	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire dedicated conn: %w", err)
	}

	var ok bool
	query := "SELECT pg_try_advisory_lock($1)"
	if blocking {
		query = "SELECT pg_advisory_lock($1)"
	}
	if err := conn.QueryRowContext(ctx, query, key).Scan(&ok); err != nil {
		_ = conn.Close()
		return nil, false, fmt.Errorf("%s: %w", query, err)
	}
	if !ok {
		_ = conn.Close()
		return nil, false, nil
	}
	return conn, true, nil
}

// unlockConn 释放锁并归还连接。
//
// 用独立超时上下文：进程收到退出信号时主 context 已被取消，若复用同一个
// context，解锁语句会立刻失败。即使解锁失败也无害——连接关闭时会话结束，
// 锁由数据库自动释放。
func unlockConn(conn *sql.Conn, key int64) {
	if conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", key); err != nil {
		fmt.Printf("warn: pg_advisory_unlock(%d) failed: %v\n", key, err)
	}
	_ = conn.Close()
}
