package store

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An unset XHS_DATABASE_URL is the default deployment, and it must cost
// nothing: no connection, no error, just the store that misses everything.
func TestOpenEmptyURLIsNop(t *testing.T) {
	s, err := Open(context.Background(), "   ")
	require.NoError(t, err)
	require.IsType(t, Nop{}, s)
}

// A URL naming a backend nobody linked is an error, never a quiet downgrade to
// Nop. An operator who configured a database has said they expect caching.
func TestOpenUnknownSchemeIsAnError(t *testing.T) {
	for _, rawURL := range []string{
		"mongodb://localhost:27017/xhs", // where a future backend would plug in
		"mysql://localhost/xhs",
		"/var/lib/xhs.db",
	} {
		t.Run(rawURL, func(t *testing.T) {
			s, err := Open(context.Background(), rawURL)
			require.Error(t, err)
			require.Nil(t, s)
		})
	}
}

func TestOpenUsesRegisteredBackend(t *testing.T) {
	want := errors.New("dialled")
	var got string
	Register("teststore", func(_ context.Context, url string) (Store, error) {
		got = url
		return nil, want
	})

	_, err := Open(context.Background(), "teststore://user:pw@example/db")
	require.ErrorIs(t, err, want)
	require.Equal(t, "teststore://user:pw@example/db", got, "the backend gets the url verbatim")
}

func TestRegisterRejectsDuplicates(t *testing.T) {
	Register("dupstore", func(context.Context, string) (Store, error) { return Nop{}, nil })
	require.Panics(t, func() {
		Register("dupstore", func(context.Context, string) (Store, error) { return Nop{}, nil })
	})
}

// A database URL reaches log lines and error messages; the password must not.
func TestRedact(t *testing.T) {
	assert.Equal(t, "postgres://xhs:xxxxx@db:5432/xhs", Redact("postgres://xhs:hunter2@db:5432/xhs"))
	assert.Equal(t, "postgres://xhs@db:5432/xhs", Redact("postgres://xhs@db:5432/xhs"))
	assert.NotContains(t, Redact("postgres://xhs:hunter2@db/xhs"), "hunter2")
}
