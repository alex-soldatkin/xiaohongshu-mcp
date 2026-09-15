package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/store"
)

// The embedded migrations are checked without a database, because the failure
// this guards against — a file named so that it sorts or parses wrongly — is
// introduced when the file is added, not when it is applied.
func TestLoadMigrations(t *testing.T) {
	migrations, err := loadMigrations()
	require.NoError(t, err)
	require.NotEmpty(t, migrations, "the initial schema must be embedded")

	for i, m := range migrations {
		require.NotEmpty(t, m.body, "%s is empty", m.name)
		if i > 0 {
			require.Greater(t, m.version, migrations[i-1].version,
				"versions must be unique and numerically ascending, not lexically")
		}
	}
	require.Equal(t, 1, migrations[0].version)
}

// The cursor is a padded decimal, and the padding is the only reason a stored
// cursor can be compared as a string. parseCursor has to accept what the SQL
// side produces.
func TestParseCursor(t *testing.T) {
	for _, tc := range []struct {
		cursor store.Cursor
		want   int64
	}{
		{"", 0},
		{"00000000000000000000", 0},
		{"00000000000000000001", 1},
		{"00000000000000000009", 9},
		{"00000000000000000010", 10},
		{"12", 12},
	} {
		got, err := parseCursor(tc.cursor)
		require.NoError(t, err, "cursor %q", tc.cursor)
		require.Equal(t, tc.want, got)
	}

	_, err := parseCursor("not-a-cursor")
	require.Error(t, err)
}

// The padded width and the width in the SQL expression have to agree; they are
// written in two places and only one of them is Go.
func TestCursorWidthMatchesSQL(t *testing.T) {
	require.Equal(t, 20, cursorWidth)
	require.Contains(t, cursorExpr, "lpad(seq::text, 20, '0')")
}

// Linking this package must teach store.Open about postgres URLs. The URL here
// fails in the driver's own parser, which is enough to prove the call was
// routed here without opening a socket.
func TestSchemesAreRegistered(t *testing.T) {
	for _, scheme := range []string{"postgres", "postgresql"} {
		_, err := store.Open(context.Background(), scheme+"://user@localhost/xhs?sslmode=nonsense")
		require.ErrorContains(t, err, "pgstore:")
	}
}

// A database that is configured but not answering must be an error from Open,
// not a store that quietly misses everything. main.go turns this into a fatal
// startup error on the grounds that an operator who set XHS_DATABASE_URL
// believes caching is working.
func TestOpenUnreachableDatabaseFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Port 1 is not something a database listens on.
	s, err := Open(ctx, "postgres://xhs@127.0.0.1:1/xhs?sslmode=disable&connect_timeout=2")
	require.Error(t, err)
	require.Nil(t, s)
}
