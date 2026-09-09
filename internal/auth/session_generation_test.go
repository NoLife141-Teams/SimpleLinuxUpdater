package auth

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
)

func TestSessionGenerationRevocationSurvivesManagerReplacement(t *testing.T) {
	for _, clear := range []string{"all", "others"} {
		t.Run(clear, func(t *testing.T) {
			db := newTestDB(t)
			service := NewService(ServiceOptions{DB: func() *sql.DB { return db }})
			sm, err := NewSessionManager(db, SessionManagerOptions{})
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := PrepareAuthentication(context.Background(), sm)
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := sm.Load(prepared, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := StageAuthentication(ctx, sm); err != nil {
				t.Fatal(err)
			}
			sm.Put(ctx, SessionUserKey, "admin")
			deadline := time.Now().Add(time.Hour)
			data, err := (scs.GobCodec{}).Encode(deadline, map[string]any{SessionUserKey: "admin", sessionLoginGenerationKey: sm.Get(ctx, sessionLoginGenerationKey)})
			if err != nil {
				t.Fatal(err)
			}
			if err := sm.Store.Commit("owner", data, deadline); err != nil {
				t.Fatal(err)
			}
			if clear == "all" {
				_, err = service.ClearSessions()
			} else {
				_, err = service.ClearOtherSessions("owner")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := EnsureSchema(db); err != nil {
				t.Fatal(err)
			}
			replacement, err := NewSessionManager(db, SessionManagerOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := replacement.Store.Commit("late-login", data, deadline); !errors.Is(err, ErrAuthenticationChanged) {
				t.Fatalf("stale new session commit: %v", err)
			}
			if _, exists, err := replacement.Store.Find("late-login"); err != nil || exists {
				t.Fatalf("late session exists=%v err=%v", exists, err)
			}
			if clear == "others" {
				if err := replacement.Store.Commit("owner", data, deadline); err != nil {
					t.Fatalf("preserved session refresh: %v", err)
				}
			}
			fresh, err := PrepareAuthentication(context.Background(), replacement)
			if err != nil {
				t.Fatal(err)
			}
			generation := fresh.Value(loginGenerationContextKey{}).(int64)
			freshData, err := (scs.GobCodec{}).Encode(deadline, map[string]any{SessionUserKey: "admin", sessionLoginGenerationKey: generation})
			if err != nil {
				t.Fatal(err)
			}
			if err := replacement.Store.Commit("fresh-login", freshData, deadline); err != nil {
				t.Fatalf("fresh login rejected: %v", err)
			}
		})
	}
}
