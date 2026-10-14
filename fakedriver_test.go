package dtmx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"testing"
)

// 本文件是 trace_test.go 的最小 `database/sql` 假驱动。
//
// 为什么自带而不引 sqlite：本仓的 cgo 在本机被环境钩子禁用（go-sqlite3 只剩
// stub），任何依赖它的测试都无法运行；而 ExecTx 的判据只需要「语句执行过」
// 这件事可观察，不需要真的落库。
type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return fakeConn{}, nil }

type fakeConn struct{}

func (fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fakeConn: 只支持直连 Exec/Query")
}
func (fakeConn) Close() error               { return nil }
func (fakeConn) Ping(context.Context) error { return nil }
func (fakeConn) Begin() (driver.Tx, error)  { return fakeTx{}, nil }

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

func (fakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("fakeConn: 空语句")
	}
	return driver.RowsAffected(1), nil
}

func (fakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("fakeConn: 空语句")
	}
	return &fakeRows{cols: []string{"v"}}, nil
}

type fakeRows struct {
	cols []string
	done bool
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(0)
	return nil
}

// beginFake 开一个带假驱动的连接并起事务，返回 tx 与必须关掉的 close 闭包。
func beginFake(t *testing.T) (*sql.Tx, func()) {
	t.Helper()
	db := sql.OpenDB(fakeConnector{})
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("起事务失败：%v", err)
	}
	return tx, func() { _ = tx.Rollback(); _ = db.Close() }
}

type fakeConnector struct{}

func (fakeConnector) Connect(context.Context) (driver.Conn, error) { return fakeConn{}, nil }
func (fakeConnector) Driver() driver.Driver                        { return fakeDriver{} }
