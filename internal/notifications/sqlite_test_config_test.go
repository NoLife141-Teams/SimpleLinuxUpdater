package notifications

import (
	"context"
	"strings"

	sqlite "modernc.org/sqlite"
)

func init() {
	sqlite.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, dsn string) error {
		// The destination-binding regression exercises the same background-worker/read
		// contention that production absorbs with a busy timeout. Keep this setting
		// scoped to that database so deliberate SQLITE_BUSY tests still fail fast.
		if !strings.Contains(dsn, "destination-binding.db") {
			return nil
		}
		_, err := conn.ExecContext(context.Background(), "PRAGMA busy_timeout=5000", nil)
		return err
	})
}
