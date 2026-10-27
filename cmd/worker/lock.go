package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// acquireSingletonLock 用 PostgreSQL advisory lock 保证同一时刻只有一个 worker 在跑。
//
// 为什么不用 SELECT ... FOR UPDATE 或状态表：advisory lock 不落任何 schema、不产生
// 脏数据、进程崩溃时由数据库会话自动释放，是「进程级单例」最省事的做法。
//
// 注意：advisory lock 是**会话级**的，必须固定在一条连接上，否则连接池换一条连接
// 就变成了另一把锁。所以这里从连接池单独取一条 *sql.Conn，并持有到进程退出。
func acquireSingletonLock(ctx context.Context, db *gorm.DB, key int64) (release func(), acquired bool, err error) {
	noop := func() {}

	if db.Dialector.Name() != "postgres" {
		// 非 PostgreSQL（例如 sqlite 下的单测）不支持 advisory lock，
		// 直接放行，单例由部署层（replicas: 1）保证。
		return noop, true, nil
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
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&ok); err != nil {
		_ = conn.Close()
		return nil, false, fmt.Errorf("pg_try_advisory_lock: %w", err)
	}

	if !ok {
		_ = conn.Close()
		return noop, false, nil
	}

	release = func() { unlockConn(conn, key) }
	return release, true, nil
}

// unlockConn 释放 advisory lock 并归还连接。用独立超时上下文，
// 保证进程收到退出信号、主 context 已取消时仍然能完成解锁。
func unlockConn(conn *sql.Conn, key int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", key); err != nil {
		// 解锁失败无需中断退出流程：连接关闭时锁会随会话结束自动释放
		fmt.Printf("warn: pg_advisory_unlock failed: %v\n", err)
	}
	_ = conn.Close()
}
