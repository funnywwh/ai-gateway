package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
)

// Prepared-statement cache for the write paths.
//
// Why this exists: the pure-Go driver prepares a statement on every Exec. A CPU profile
// of the running 8088 instance showed the SQLite parser on every request
// (`sqlite3Prepare` -> `sqlite3RunParser` -> `yy_reduce` and the VDBE variable binding),
// spent re-parsing the same three fixed INSERTs.
//
// The cache holds `*sql.Stmt`, not a driver statement. That matters for correctness: a
// driver statement belongs to one driver connection, but the pool is free to replace that
// connection (a bad connection is discarded, `SetConnMaxLifetime` rotates one too).
// `database/sql` keeps, per statement, one entry per connection and re-prepares on a new
// connection by itself — so a rotated connection costs a re-prepare, never a wrong query.
// Nothing here needs the connection's identity, which the driver does not expose anyway.
type writerStmtCache struct {
	mu    sync.Mutex
	stmts map[string]*sql.Stmt
}

func newWriterStmtCache() *writerStmtCache {
	return &writerStmtCache{stmts: make(map[string]*sql.Stmt, 8)}
}

// do runs one write with a cached statement for sqlText. The arguments are the same
// positional values the call site would have passed to ExecContext.
//
// Retry rule: the cached statement can be closed underneath us (its connection was
// discarded). That surfaces as sql.ErrStmtClosed / driver.ErrBadConn, and both mean the
// same thing here — drop the cache entry and prepare again, exactly once. A second
// failure is a real error and is returned to the caller with the statement dropped, so
// the next request starts from a clean cache instead of a permanently closed one.
func (c *writerStmtCache) do(ctx context.Context, db *sql.DB, sqlText string, args ...any) (sql.Result, error) {
	stmt, err := c.stmt(ctx, db, sqlText)
	if err != nil {
		return nil, err
	}

	res, err := stmt.ExecContext(ctx, args...)
	if err == nil || !statementUnusable(err) {
		return res, err
	}

	c.drop(sqlText)
	stmt, rerr := c.stmt(ctx, db, sqlText)
	if rerr != nil {
		return nil, fmt.Errorf("%w (after re-preparing: %v)", err, rerr)
	}
	return stmt.ExecContext(ctx, args...)
}

// execInTx runs sqlText inside a transaction with a statement prepared on that
// transaction's connection.
//
// Why not reuse the pool-level statement through tx.StmtContext: with a single-connection
// writer pool that deadlocks. A statement prepared on the pool holds that one connection
// while it exists, and tx.StmtContext then asks the statement's connection group for the
// same connection — which the statement itself is holding. Reproduced in
// stmtcache_tx_test.go: the very first execution times out.
//
// So a transaction gets its own statement. Within one batch that is still one parse
// instead of one per row; across batches it costs one parse per transaction, which is
// what the caller was paying per statement before.
func (c *writerStmtCache) execInTx(ctx context.Context, db *sql.DB, tx *sql.Tx, sqlText string, args ...any) (sql.Result, error) {
	_ = db // kept for symmetry with do; the transaction owns the connection here
	stmt, err := tx.PrepareContext(context.WithoutCancel(ctx), sqlText)
	if err != nil {
		return nil, fmt.Errorf("store: prepare %s in transaction: %w", firstLine(sqlText), err)
	}
	defer func() { _ = stmt.Close() }()

	res, err := stmt.ExecContext(ctx, args...)
	if err == nil || !statementUnusable(err) {
		return res, err
	}
	return nil, fmt.Errorf("store: execute %s in transaction: %w", firstLine(sqlText), err)
}

func (c *writerStmtCache) stmt(ctx context.Context, db *sql.DB, sqlText string) (*sql.Stmt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// A cached statement whose context was cancelled would fail every later execution, so
	// prepare against a context that only lives as long as the statement itself.
	if stmt, ok := c.stmts[sqlText]; ok {
		return stmt, nil
	}
	stmt, err := db.PrepareContext(context.WithoutCancel(ctx), sqlText)
	if err != nil {
		return nil, fmt.Errorf("store: prepare %s: %w", firstLine(sqlText), err)
	}
	c.stmts[sqlText] = stmt
	return stmt, nil
}

func (c *writerStmtCache) drop(sqlText string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if stmt, ok := c.stmts[sqlText]; ok {
		_ = stmt.Close()
		delete(c.stmts, sqlText)
	}
}

// Close releases every cached statement. The pool must not be closed first, otherwise
// database/sql has already closed them and this is a no-op.
func (c *writerStmtCache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, stmt := range c.stmts {
		_ = stmt.Close()
		delete(c.stmts, key)
	}
}

// errStmtClosed mirrors the unexported error database/sql returns from a closed
// statement ("sql: statement is closed", sql.go connStmt). It is not exported, and it is
// the one signal that the cached statement is gone for good, so it is matched by value
// here rather than by string.
var errStmtClosed = errors.New("sql: statement is closed")

// statementUnusable reports whether err means "this prepared statement (or its
// connection) is gone", which is the only case worth re-preparing for.
func statementUnusable(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, errStmtClosed) ||
		errors.Is(err, sql.ErrConnDone) ||
		errors.Is(err, driver.ErrBadConn)
}

// firstLine keeps error messages readable when the SQL is a multi-line literal.
func firstLine(sqlText string) string {
	for i := 0; i < len(sqlText); i++ {
		if sqlText[i] == '\n' {
			return sqlText[:i] + " …"
		}
	}
	return sqlText
}
