// Package duckconn opens the DuckDB databases the engine reads and
// writes: gold, a compaction's scratch database and the one silver that
// is DuckDB.
package duckconn

import (
	"context"
	"database/sql"
	"database/sql/driver"

	"github.com/duckdb/duckdb-go/v2"
)

// Open opens the DuckDB database dsn names, a path or ":memory:" with
// DuckDB's options as `?key=value` parameters. The database is opened
// before Open returns, so a file that is missing, damaged or locked
// fails here and not at the first query.
//
// Every session runs in UTC. DuckDB otherwise takes the time zone of
// the machine, and a cast of an instant to a day, or a comparison of an
// instant with a plain timestamp, then follows that zone. The engine's
// days are UTC days on any machine. The setting is made on each new
// connection, since DuckDB loads its time zone support after it has
// read a DSN's parameters.
func Open(dsn string) (*sql.DB, error) {
	c, err := duckdb.NewConnector(dsn, func(conn driver.ExecerContext) error {
		_, err := conn.ExecContext(context.Background(), `SET TimeZone = 'UTC'`, nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(c), nil
}
