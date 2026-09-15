package store

import (
	"context"
	"time"
)

// Nop is the store used when XHS_DATABASE_URL is unset, which is the default
// for a single-user local MCP server. Every read misses, every write is
// accepted and discarded, every listing is empty.
//
// It exists so that the cache layer has one code path rather than a nil check
// at every call site. The cache layer is still expected to short-circuit on a
// disabled store before marshalling anything: with Nop in place the unset
// deployment must cost one boolean test, not a JSON round trip into a bin.
//
// The zero value is ready to use and it holds no state, so it is trivially
// safe for concurrent use.
type Nop struct{}

// Nop implements Store.
var _ Store = Nop{}

func (Nop) GetDoc(context.Context, string, Kind, string) (Doc, error) {
	return Doc{}, ErrNotFound
}

func (Nop) PutDoc(context.Context, string, Kind, string, Doc) error { return nil }

func (Nop) DeleteDocs(context.Context, string, Kind, ...string) error { return nil }

func (Nop) AppendNotifications(context.Context, string, []NotificationRecord) (int, error) {
	return 0, nil
}

func (Nop) NotificationsSince(context.Context, string, string, Cursor, int) ([]HistoryItem, error) {
	return nil, nil
}

func (Nop) AppendComments(context.Context, string, []CommentRecord) (int, error) { return 0, nil }

func (Nop) CommentsSince(context.Context, string, string, Cursor, int) ([]HistoryItem, error) {
	return nil, nil
}

func (Nop) UpsertAccount(context.Context, Account) error { return nil }

func (Nop) AccountBySeed(context.Context, int) (Account, error) { return Account{}, ErrNotFound }

func (Nop) PruneDocs(context.Context, time.Duration) (int64, error) { return 0, nil }

func (Nop) Close() error { return nil }
