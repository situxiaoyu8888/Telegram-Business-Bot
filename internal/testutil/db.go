// Package testutil provides a scripted SQL connection for deterministic service regression tests.
package testutil

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"io"
	"strings"
	"sync"
	"testing"
)

type Step struct {
	Contains string
	Args     []any
	Columns  []string
	Rows     [][]driver.Value
	Affected int64
	Err      error
}
type script struct {
	mu    sync.Mutex
	t     testing.TB
	steps []Step
}
type connector struct{ s *script }
type conn struct{ s *script }
type transaction struct{}
type rows struct {
	columns []string
	data    [][]driver.Value
	offset  int
}
type drv struct{}

func (drv) Open(string) (driver.Conn, error)                     { return nil, fmt.Errorf("use connector") }
func (c connector) Connect(context.Context) (driver.Conn, error) { return &conn{c.s}, nil }
func (c connector) Driver() driver.Driver                        { return drv{} }
func (c *conn) Prepare(string) (driver.Stmt, error)              { return nil, fmt.Errorf("unexpected prepare") }
func (c *conn) Close() error                                     { return nil }
func (c *conn) Begin() (driver.Tx, error)                        { return transaction{}, nil }
func (transaction) Commit() error                                { return nil }
func (transaction) Rollback() error                              { return nil }
func (c *conn) next(query string, args []driver.NamedValue) Step {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if len(c.s.steps) == 0 {
		c.s.t.Errorf("unexpected SQL: %s", query)
		return Step{Err: fmt.Errorf("unexpected SQL")}
	}
	step := c.s.steps[0]
	c.s.steps = c.s.steps[1:]
	if !strings.Contains(query, step.Contains) {
		c.s.t.Errorf("SQL %q does not contain %q", query, step.Contains)
	}
	if step.Args != nil {
		if len(args) != len(step.Args) {
			c.s.t.Errorf("SQL %s args = %v, want %v", query, args, step.Args)
		} else {
			for i, a := range args {
				if fmt.Sprint(a.Value) != fmt.Sprint(step.Args[i]) {
					c.s.t.Errorf("arg %d=%v want %v for %s", i, a.Value, step.Args[i], query)
				}
			}
		}
	}
	return step
}
func (c *conn) ExecContext(_ context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	s := c.next(q, a)
	return result{s.Affected}, s.Err
}
func (c *conn) QueryContext(_ context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	s := c.next(q, a)
	return &rows{columns: s.Columns, data: s.Rows}, s.Err
}

type result struct{ affected int64 }

func (result) LastInsertId() (int64, error)   { return 1, nil }
func (r result) RowsAffected() (int64, error) { return r.affected, nil }
func (r *rows) Columns() []string             { return r.columns }
func (r *rows) Close() error                  { return nil }
func (r *rows) Next(dest []driver.Value) error {
	if r.offset >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.offset])
	r.offset++
	return nil
}

func DB(t testing.TB, steps ...Step) *gorm.DB {
	t.Helper()
	script := &script{t: t, steps: steps}
	db := sql.OpenDB(connector{script})
	g, err := gorm.Open(mysql.New(mysql.Config{Conn: db, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		script.mu.Lock()
		defer script.mu.Unlock()
		if len(script.steps) > 0 {
			t.Errorf("%d SQL steps not executed, next: %s", len(script.steps), script.steps[0].Contains)
		}
	})
	return g
}
