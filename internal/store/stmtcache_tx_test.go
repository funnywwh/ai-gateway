package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/config"
)

// 复现并锁定一个真实缺陷：池级预编译语句通过 tx.StmtContext 执行会**自锁**。
// 写连接池只有 1 条连接（write.SetMaxOpenConns(1)），tx.StmtContext 会去要
// 「语句所属连接组」的那条连接，而语句本身就占着它 —— 第一次执行就超时。
// execInTx 因此改为在事务自己的连接上 prepare；本测试守住这条路。
func TestStmtCacheInsideTxDoesNotDeadlock(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Database.Path = filepath.Join(t.TempDir(), "stmt.db")
	db, err := Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const ddl = `CREATE TABLE IF NOT EXISTS probe(id INTEGER PRIMARY KEY, v TEXT)`
	if _, err := db.write.ExecContext(ctx, ddl); err != nil {
		t.Fatal(err)
	}

	// 先让缓存里有一条池级语句，制造出会自锁的前提。
	if _, err := db.stmts.do(ctx, db.write, `INSERT INTO probe(id, v) VALUES(?, ?)`, -1, "pool"); err != nil {
		t.Fatalf("pool-level statement: %v", err)
	}

	for i := 0; i < 3; i++ {
		tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		tx, err := db.write.BeginTx(tctx, nil)
		if err != nil {
			cancel()
			t.Fatalf("round %d: begin: %v", i, err)
		}
		if _, err := db.stmts.execInTx(tctx, db.write, tx, `INSERT INTO probe(id, v) VALUES(?, ?)`, i, "tx"); err != nil {
			cancel()
			t.Fatalf("round %d: exec in tx: %v", i, err)
		}
		if err := tx.Commit(); err != nil {
			cancel()
			t.Fatalf("round %d: commit: %v", i, err)
		}
		cancel()
	}

	var n int
	if err := db.read.QueryRowContext(ctx, `SELECT count(*) FROM probe`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("rows = %d, want 4", n)
	}
}
