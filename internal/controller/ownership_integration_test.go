package controller

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// TestOwnershipRepair uses a disposable database, never an application database.
// OWNERSHIP_TEST_DSN must point to an empty database owned by a test superuser.
func TestOwnershipRepair(t *testing.T) {
	dsn := os.Getenv("OWNERSHIP_TEST_DSN")
	if dsn == "" {
		t.Skip("set OWNERSHIP_TEST_DSN to an isolated, disposable PostgreSQL database")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	exec := func(query string) {
		t.Helper()
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	// Match the namespace/database role naming used by Authentik.
	owner := "test_owner-with-dash"
	exec(`CREATE ROLE "test_owner-with-dash"`)
	exec(`CREATE TABLE public.owned_table (id integer)`)
	exec(`CREATE TABLE public.other_table (id integer)`)
	exec(`CREATE SEQUENCE public.owned_sequence`)
	exec(`CREATE VIEW public.owned_view AS SELECT id FROM public.owned_table`)
	exec(`CREATE MATERIALIZED VIEW public.owned_matview AS SELECT id FROM public.owned_table`)
	exec(`CREATE FUNCTION public.owned_function() RETURNS integer LANGUAGE sql AS 'SELECT 1'`)
	exec(`CREATE TYPE public.owned_enum AS ENUM ('one', 'two')`)
	exec(`CREATE TYPE public.owned_composite AS (id integer)`)
	exec(`CREATE DOMAIN public.owned_domain AS integer`)

	t.Run("repair all supported objects and repeat without changes", func(t *testing.T) {
		for range 2 {
			if err := transferObjectOwnership(context.Background(), db, owner, logr.Discard()); err != nil {
				t.Fatal(err)
			}
		}
		var wrongOwners int
		err := db.QueryRow(`SELECT count(*) FROM (
			SELECT relowner AS owner FROM pg_class WHERE relnamespace = 'public'::regnamespace
			UNION ALL SELECT proowner FROM pg_proc WHERE pronamespace = 'public'::regnamespace
			UNION ALL SELECT typowner FROM pg_type WHERE typnamespace = 'public'::regnamespace
		) objects WHERE owner <> (SELECT oid FROM pg_roles WHERE rolname = $1)`, owner).Scan(&wrongOwners)
		if err != nil || wrongOwners != 0 {
			t.Fatalf("ownership not repaired: mismatches=%d, error=%v", wrongOwners, err)
		}
	})

	t.Run("already owned table does not queue behind a migration", func(t *testing.T) {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`LOCK TABLE public.owned_table IN ACCESS SHARE MODE`); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := transferObjectOwnership(ctx, db, owner, logr.Discard()); err != nil {
			t.Fatal(err)
		}
		if ctx.Err() != nil {
			t.Fatal("no-op ownership reconcile waited for an application lock")
		}
	})

	t.Run("wrong owner times out, releases locks and succeeds on retry", func(t *testing.T) {
		exec(`ALTER TABLE public.other_table OWNER TO CURRENT_USER`)
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`LOCK TABLE public.other_table IN ACCESS SHARE MODE`); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		if err := transferObjectOwnership(ctx, db, owner, logr.Discard()); err == nil {
			t.Error("failed ownership repair must return an error for controller retry")
		}
		if elapsed := time.Since(start); elapsed > 4*time.Second {
			t.Errorf("ownership repair waited %s; lock timeout should end it in about two seconds", elapsed)
		}
		if _, err := tx.Exec(`SELECT id FROM public.other_table LIMIT 1`); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if err := transferObjectOwnership(context.Background(), db, owner, logr.Discard()); err != nil {
			t.Fatal(err)
		}
	})
}
