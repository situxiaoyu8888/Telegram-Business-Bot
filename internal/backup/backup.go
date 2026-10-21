// Package backup writes a restorable SQL dump of the MySQL/MariaDB database, like mysqldump
// but without needing the client tools in the container.
package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

const rowsPerInsert = 200

// Dump writes every table's definition and rows as SQL. All reads come from one consistent
// snapshot, so a dump taken while orders are being placed is still internally consistent.
// Restore with: gunzip -c backup.sql.gz | mysql -u USER -p DBNAME
func Dump(ctx context.Context, db *sql.DB, w io.Writer) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "START TRANSACTION WITH CONSISTENT SNAPSHOT"); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK") //nolint:errcheck

	tables, err := baseTables(ctx, conn)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "-- telegram-business-bot backup %s\nSET NAMES utf8mb4;\nSET FOREIGN_KEY_CHECKS=0;\nSET UNIQUE_CHECKS=0;\n\n", time.Now().Format(time.RFC3339))
	for _, t := range tables {
		if err := dumpTable(ctx, conn, w, t); err != nil {
			return fmt.Errorf("table %s: %w", t, err)
		}
	}
	_, err = io.WriteString(w, "SET UNIQUE_CHECKS=1;\nSET FOREIGN_KEY_CHECKS=1;\n")
	return err
}

func baseTables(ctx context.Context, conn *sql.Conn) ([]string, error) {
	rows, err := conn.QueryContext(ctx, "SHOW FULL TABLES WHERE Table_type = 'BASE TABLE'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, rows.Err()
}

func quoteIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

func dumpTable(ctx context.Context, conn *sql.Conn, w io.Writer, table string) error {
	var name, create string
	if err := conn.QueryRowContext(ctx, "SHOW CREATE TABLE "+quoteIdent(table)).Scan(&name, &create); err != nil {
		return err
	}
	fmt.Fprintf(w, "DROP TABLE IF EXISTS %s;\n%s;\n\n", quoteIdent(table), create)

	// Learn the column types first. Date and time columns are then read as text by the server:
	// with parseTime in the DSN the driver would otherwise reformat them (RFC 3339), which
	// MySQL does not accept back on restore.
	probe, err := conn.QueryContext(ctx, "SELECT * FROM "+quoteIdent(table)+" LIMIT 0")
	if err != nil {
		return err
	}
	types, err := probe.ColumnTypes()
	probe.Close()
	if err != nil {
		return err
	}
	cols := make([]string, len(types))
	selects := make([]string, len(types))
	binary := make([]bool, len(types))
	for i, t := range types {
		cols[i] = quoteIdent(t.Name())
		selects[i] = cols[i]
		tn := strings.ToUpper(t.DatabaseTypeName())
		binary[i] = strings.Contains(tn, "BLOB") || strings.Contains(tn, "BINARY") || tn == "BIT"
		if tn == "DATETIME" || tn == "TIMESTAMP" || tn == "DATE" || tn == "TIME" || tn == "YEAR" {
			selects[i] = "CAST(" + cols[i] + " AS CHAR)"
		}
	}
	rows, err := conn.QueryContext(ctx, "SELECT "+strings.Join(selects, ",")+" FROM "+quoteIdent(table))
	if err != nil {
		return err
	}
	defer rows.Close()
	head := fmt.Sprintf("INSERT INTO %s (%s) VALUES\n", quoteIdent(table), strings.Join(cols, ","))
	vals := make([]sql.RawBytes, len(types))
	ptrs := make([]any, len(types))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var batch bytes.Buffer
	n := 0
	flush := func() error {
		if n == 0 {
			return nil
		}
		if _, err := io.WriteString(w, head); err != nil {
			return err
		}
		batch.WriteString(";\n")
		_, err := w.Write(batch.Bytes())
		batch.Reset()
		n = 0
		return err
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		if n > 0 {
			batch.WriteString(",\n")
		}
		batch.WriteByte('(')
		for i, v := range vals {
			if i > 0 {
				batch.WriteByte(',')
			}
			switch {
			case v == nil:
				batch.WriteString("NULL")
			case binary[i]:
				fmt.Fprintf(&batch, "X'%x'", []byte(v))
			default:
				batch.WriteString(quoteString(v))
			}
		}
		batch.WriteByte(')')
		n++
		if n == rowsPerInsert {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n")
	return err
}

// quoteString renders a value as a MySQL string literal; numbers and dates are accepted as strings.
func quoteString(b []byte) string {
	var s strings.Builder
	s.Grow(len(b) + 2)
	s.WriteByte('\'')
	for _, c := range b {
		switch c {
		case 0:
			s.WriteString(`\0`)
		case '\n':
			s.WriteString(`\n`)
		case '\r':
			s.WriteString(`\r`)
		case '\\':
			s.WriteString(`\\`)
		case '\'':
			s.WriteString(`\'`)
		case 0x1a:
			s.WriteString(`\Z`)
		default:
			s.WriteByte(c)
		}
	}
	s.WriteByte('\'')
	return s.String()
}

// WriteGzip writes a gzip-compressed dump.
func WriteGzip(ctx context.Context, db *sql.DB, w io.Writer) error {
	gz := gzip.NewWriter(w)
	if err := Dump(ctx, db, gz); err != nil {
		gz.Close()
		return err
	}
	return gz.Close()
}

// FileName is the name of a backup taken at t.
func FileName(t time.Time) string { return "backup-" + t.Format("20060102-150405") + ".sql.gz" }

// ToDir writes a backup into dir (via a temporary file, so a crash never leaves a partial
// backup under the final name) and keeps only the newest keep backups.
func ToDir(ctx context.Context, db *sql.DB, dir string, keep int, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	final := filepath.Join(dir, FileName(now))
	tmp, err := os.CreateTemp(dir, ".backup-*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := WriteGzip(ctx, db, tmp); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return "", err
	}
	return final, prune(dir, keep)
}

func prune(dir string, keep int) error {
	if keep < 1 {
		return nil
	}
	files, err := filepath.Glob(filepath.Join(dir, "backup-*.sql.gz"))
	if err != nil {
		return err
	}
	sort.Strings(files) // names sort by time
	for len(files) > keep {
		if err := os.Remove(files[0]); err != nil {
			return err
		}
		files = files[1:]
	}
	return nil
}

// RunDaily backs up to dir once a day at hour (server time) until ctx ends. A Redis key per date
// keeps several replicas from all writing the same backup.
func RunDaily(ctx context.Context, db *sql.DB, rdb *redis.Client, dir string, keep, hour int, logger *zap.Logger) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		now := time.Now()
		if now.Hour() == hour {
			ok, err := rdb.SetNX(ctx, "backup:"+now.Format("2006-01-02"), "1", 26*time.Hour).Result()
			if err == nil && ok {
				if path, err := ToDir(ctx, db, dir, keep, now); err != nil {
					logger.Error("daily backup failed; will retry", zap.Error(err))
					rdb.Del(context.Background(), "backup:"+now.Format("2006-01-02"))
				} else {
					logger.Info("daily backup written", zap.String("file", path))
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
