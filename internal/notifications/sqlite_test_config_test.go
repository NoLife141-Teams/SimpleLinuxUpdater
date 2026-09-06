package notifications

import (
	"context"

	sqlite "modernc.org/sqlite"
)

func init() {
	sqlite.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
		_, err := conn.ExecContext(context.Background(), "PRAGMA busy_timeout=5000", nil)
		return err
	})
}
