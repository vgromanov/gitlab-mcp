package intentstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"unsafe"

	"modernc.org/libc"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func init() {
	sqlite.MustRegisterScalarFunction(
		"intentstore_flush_pages",
		0,
		func(ctx *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
			if err := flushConnection(ctx); err != nil {
				return nil, err
			}
			return int64(1), nil
		},
	)
}

// flushConnection writes dirty pages into the WAL without committing.
// The view matches FunctionContext in modernc.org/sqlite v1.59.0.
func flushConnection(ctx *sqlite.FunctionContext) error {
	type view struct {
		tls *libc.TLS
		ctx uintptr
	}
	v := (*view)(unsafe.Pointer(ctx))
	db := sqlite3.Xsqlite3_context_db_handle(v.tls, v.ctx)
	if db == 0 {
		return fmt.Errorf("intent store: flush pages: no database")
	}
	rc := sqlite3.Xsqlite3_db_cacheflush(v.tls, db)
	if rc != sqlite3.SQLITE_OK {
		return fmt.Errorf("intent store: flush pages: sqlite %d", rc)
	}
	return nil
}

func flushPages(ctx context.Context, tx *sql.Tx) error {
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT intentstore_flush_pages()`).Scan(&n); err != nil {
		return mapDriver(err)
	}
	return nil
}
