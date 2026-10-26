package dashboardfixture

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"
	"sync/atomic"

	"modernc.org/sqlite"
)

// SQLMeter counts actual statement executions, including prepared statements.
// Reset only when no other request is running. It deliberately excludes row
// iteration time; wall time includes that work at the service boundary.
type SQLMeter struct {
	mu         sync.Mutex
	Statements map[string]int
}

func (m *SQLMeter) record(q string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Statements == nil {
		m.Statements = map[string]int{}
	}
	m.Statements[q]++
}
func (m *SQLMeter) Reset() { m.mu.Lock(); defer m.mu.Unlock(); m.Statements = map[string]int{} }
func (m *SQLMeter) Snapshot() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := map[string]int{}
	for q, n := range m.Statements {
		r[q] = n
	}
	return r
}
func (m *SQLMeter) Count() int {
	n := 0
	for _, v := range m.Snapshot() {
		n += v
	}
	return n
}

var driverID atomic.Int64

type meteredDriver struct {
	base  driver.Driver
	meter *SQLMeter
}

func (d meteredDriver) Open(name string) (driver.Conn, error) {
	c, e := d.base.Open(name)
	if e != nil {
		return nil, e
	}
	return meteredConn{Conn: c, meter: d.meter}, nil
}

type meteredConn struct {
	driver.Conn
	meter *SQLMeter
}

func (c meteredConn) Prepare(q string) (driver.Stmt, error) {
	s, e := c.Conn.Prepare(q)
	if e != nil {
		return nil, e
	}
	return meteredStmt{Stmt: s, q: q, meter: c.meter}, nil
}
func (c meteredConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	qc, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	r, e := qc.QueryContext(ctx, q, args)
	if e != driver.ErrSkip {
		c.meter.record(q)
	}
	return r, e
}
func (c meteredConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	ec, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	r, e := ec.ExecContext(ctx, q, args)
	if e != driver.ErrSkip {
		c.meter.record(q)
	}
	return r, e
}

type meteredStmt struct {
	driver.Stmt
	q     string
	meter *SQLMeter
}

func (s meteredStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), namedValues(args))
}
func (s meteredStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), namedValues(args))
}
func namedValues(args []driver.Value) []driver.NamedValue {
	result := make([]driver.NamedValue, len(args))
	for i, value := range args {
		result[i] = driver.NamedValue{Ordinal: i + 1, Value: value}
	}
	return result
}
func (s meteredStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	s.meter.record(s.q)
	return s.Stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
}
func (s meteredStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	s.meter.record(s.q)
	return s.Stmt.(driver.StmtExecContext).ExecContext(ctx, args)
}
func Open(path string) (*sql.DB, *SQLMeter, error) {
	m := &SQLMeter{}
	name := fmt.Sprintf("scaling-sqlite-%d", driverID.Add(1))
	sql.Register(name, meteredDriver{base: &sqlite.Driver{}, meter: m})
	db, e := sql.Open(name, path)
	if e == nil {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	}
	return db, m, e
}
