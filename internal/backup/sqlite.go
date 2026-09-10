package backup

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// defaultOpenSnapshot opens a SQLite file for snapshotting or verification. VACUUM INTO
// needs write access (it creates the target), while verification only reads, so the
// caller decides by opening read-only when it only inspects.
func defaultOpenSnapshot(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)", path)
	pool, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(1)
	pool.SetConnMaxLifetime(time.Minute)
	if err := pool.Ping(); err != nil {
		_ = pool.Close()
		return nil, err
	}
	return pool, nil
}
