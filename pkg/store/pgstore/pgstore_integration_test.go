//go:build integration

// The Postgres backend is exercised against a real database, or not at all.
// There is no fake connection and no testcontainers: the things worth checking
// here — ON CONFLICT counts, bigserial ordering, timestamptz rounding — are
// exactly the things a fake would get wrong in the same direction as the code.
//
// Run it with:
//
//	XHS_TEST_DATABASE_URL=postgres://user@localhost:5432/xhs_test \
//	    go test -tags integration ./pkg/store/pgstore/...
//
// Without the variable every test here skips, so `go test ./...` stays
// hermetic and green on a machine with no database.
package pgstore

import (
	"context"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/store"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/store/storetest"
)

func baseURL(t *testing.T) string {
	t.Helper()

	rawURL := os.Getenv("XHS_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("XHS_TEST_DATABASE_URL is not set; skipping the Postgres contract suite")
	}
	return rawURL
}

func testContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// freshSchemaURL gives one test its own empty schema, dropped afterwards.
// storetest.Run requires an empty, isolated store per subtest and cleans up
// nothing itself; a schema is the cheapest unit of isolation that still
// exercises the real DDL.
func freshSchemaURL(t *testing.T, ctx context.Context) string {
	t.Helper()

	rawURL := baseURL(t)
	admin, err := pgx.Connect(ctx, rawURL)
	require.NoError(t, err, "connect to XHS_TEST_DATABASE_URL")
	defer func() { _ = admin.Close(context.Background()) }()

	schema := fmt.Sprintf("xhs_test_%d_%d", time.Now().UnixNano(), rand.Intn(1000))
	_, err = admin.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %s", pgx.Identifier{schema}.Sanitize()))
	require.NoError(t, err)

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		conn, err := pgx.Connect(cleanupCtx, rawURL)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(context.Background()) }()
		_, _ = conn.Exec(cleanupCtx, fmt.Sprintf("DROP SCHEMA %s CASCADE", pgx.Identifier{schema}.Sanitize()))
	})

	// search_path is an ordinary runtime parameter, so pgx passes it through
	// in the startup packet and the migrations land in the test's own schema.
	parsed, err := url.Parse(rawURL)
	require.NoError(t, err)
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// The whole point of the contract suite: the Postgres backend passes it
// unmodified, or it is not a backend.
func TestPgstoreSatisfiesTheContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		ctx := testContext(t)
		s, err := Open(ctx, freshSchemaURL(t, ctx))
		require.NoError(t, err)
		return s
	})
}

// Two containers start together on a deploy and both run the migrations. The
// second must find nothing to do and must not double-apply anything.
func TestMigrationsAreIdempotent(t *testing.T) {
	ctx := testContext(t)
	dsn := freshSchemaURL(t, ctx)

	migrations, err := loadMigrations()
	require.NoError(t, err)
	want := migrations[len(migrations)-1].version

	first, err := Open(ctx, dsn)
	require.NoError(t, err)
	version, err := SchemaVersion(ctx, first.Pool())
	require.NoError(t, err)
	require.Equal(t, want, version)
	require.NoError(t, first.Close())

	// Applying the same set again is a read of schema_migrations and nothing
	// else. A migration that is not guarded would fail here.
	second, err := Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close() })

	version, err = SchemaVersion(ctx, second.Pool())
	require.NoError(t, err)
	require.Equal(t, want, version)

	var rows int
	require.NoError(t, second.Pool().QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&rows))
	require.Equal(t, len(migrations), rows, "one row per migration, applied once")
}

// SchemaVersion has to answer on a database that has never been migrated,
// because that is when an operator asks.
func TestSchemaVersionOnUnmigratedSchema(t *testing.T) {
	ctx := testContext(t)
	dsn := freshSchemaURL(t, ctx)

	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { _ = conn.Close(context.Background()) }()

	version, err := SchemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, 0, version)
}

// The advisory lock is the reason the runner exists in this shape. Several
// processes migrating one database at the same moment is the ordinary case on
// a deploy, and without the lock they all read version 0 and all run the DDL.
func TestMigrationsToleratePlainRacing(t *testing.T) {
	ctx := testContext(t)
	dsn := freshSchemaURL(t, ctx)

	const racers = 4
	errs := make(chan error, racers)
	for i := 0; i < racers; i++ {
		go func() {
			s, err := Open(ctx, dsn)
			if err == nil {
				_ = s.Close()
			}
			errs <- err
		}()
	}
	for i := 0; i < racers; i++ {
		require.NoError(t, <-errs)
	}

	s, err := Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	migrations, err := loadMigrations()
	require.NoError(t, err)
	var rows int
	require.NoError(t, s.Pool().QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&rows))
	require.Equal(t, len(migrations), rows)
}

// The payload column is `json`, not `jsonb`, because nothing indexes inside a
// payload. The visible consequence is that the bytes come back as they went
// in — a debugging convenience, deliberately not promised by the contract,
// which compares payloads as JSON.
func TestPayloadBytesSurviveTheJSONColumn(t *testing.T) {
	ctx := testContext(t)
	s, err := Open(ctx, freshSchemaURL(t, ctx))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	payload := `{"z":1,"a":{"nested":  [1,2,3]},"b":"  spaced  "}`
	require.NoError(t, s.PutDoc(ctx, "acct", store.KindNote, "n1", store.Doc{
		Payload: []byte(payload), FetchedAt: time.Now(),
	}))

	got, err := s.GetDoc(ctx, "acct", store.KindNote, "n1")
	require.NoError(t, err)
	require.Equal(t, payload, string(got.Payload))
}
