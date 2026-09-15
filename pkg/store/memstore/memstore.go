// Package memstore is an in-memory store.Store, for tests and for reasoning
// about the contract without a database.
//
// It is not a production backend and there is deliberately no "memory://" URL
// that would let one be configured by accident: everything it holds dies with
// the process, which defeats the point of the store.
//
// Where a behaviour is under-determined by the interface, memstore imitates
// the strictest backend rather than the most convenient one — timestamps are
// truncated to microseconds and moved to UTC the way a Postgres timestamptz
// would, and payload bytes are copied in and out so that a caller mutating its
// own buffer cannot reach into stored state. A test that passes here should
// therefore not start failing when it is pointed at a real database.
package memstore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/xpzouying/xiaohongshu-mcp/pkg/store"
)

// emptyMeta is what a nil or empty Meta is normalised to, matching the JSONB
// column default.
var emptyMeta = json.RawMessage(`{}`)

type docKey struct {
	account string
	kind    store.Kind
	key     string
}

type histKey struct {
	account string
	// scope is the tab for notifications and the note id for comments.
	scope string
	id    string
}

type histRow struct {
	seq       uint64
	payload   json.RawMessage
	firstSeen time.Time
}

// Store is an in-memory store.Store. Use New; the zero value is not usable.
type Store struct {
	mu sync.RWMutex

	docs     map[docKey]store.Doc
	accounts map[string]store.Account

	// notifications and comments are separate streams with separate cursor
	// namespaces, mirroring the two tables. The sequence counter is shared,
	// which is allowed: the contract asks for ordering within a stream, not
	// for gapless numbering.
	notifications map[histKey]*histRow
	comments      map[histKey]*histRow
	seq           uint64

	// now is overridable so retention and last-seen ordering can be tested
	// without sleeping.
	now func() time.Time
}

// Store implements store.Store.
var _ store.Store = (*Store)(nil)

// New returns an empty in-memory store.
func New() *Store {
	return &Store{
		docs:          map[docKey]store.Doc{},
		accounts:      map[string]store.Account{},
		notifications: map[histKey]*histRow{},
		comments:      map[histKey]*histRow{},
		now:           time.Now,
	}
}

// SetClock replaces the time source. Intended for tests that need to age rows
// without sleeping.
func (s *Store) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// normalizeTime matches what a timestamptz round trip does to a Go time: the
// monotonic reading is dropped, the location becomes UTC and the value is
// truncated to microsecond resolution.
func normalizeTime(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}

func cloneJSON(b json.RawMessage) json.RawMessage {
	if b == nil {
		return nil
	}
	out := make(json.RawMessage, len(b))
	copy(out, b)
	return out
}

// formatCursor renders a sequence number as an opaque, fixed-width, therefore
// lexicographically ordered cursor. The width is what makes string comparison
// agree with numeric comparison; a bare decimal would order "10" before "9".
func formatCursor(seq uint64) store.Cursor {
	return store.Cursor(fmt.Sprintf("%020d", seq))
}

func (s *Store) GetDoc(_ context.Context, account string, kind store.Kind, key string) (store.Doc, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	doc, ok := s.docs[docKey{account, kind, key}]
	if !ok {
		return store.Doc{}, store.ErrNotFound
	}
	return store.Doc{
		Payload:   cloneJSON(doc.Payload),
		Meta:      cloneJSON(doc.Meta),
		FetchedAt: doc.FetchedAt,
	}, nil
}

func (s *Store) PutDoc(_ context.Context, account string, kind store.Kind, key string, doc store.Doc) error {
	meta := cloneJSON(doc.Meta)
	if len(meta) == 0 {
		meta = cloneJSON(emptyMeta)
	}
	fetchedAt := doc.FetchedAt
	if fetchedAt.IsZero() {
		fetchedAt = s.clock()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[docKey{account, kind, key}] = store.Doc{
		Payload:   cloneJSON(doc.Payload),
		Meta:      meta,
		FetchedAt: normalizeTime(fetchedAt),
	}
	return nil
}

func (s *Store) DeleteDocs(_ context.Context, account string, kind store.Kind, keys ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(keys) == 0 {
		for k := range s.docs {
			if k.account == account && k.kind == kind {
				delete(s.docs, k)
			}
		}
		return nil
	}
	for _, key := range keys {
		delete(s.docs, docKey{account, kind, key})
	}
	return nil
}

func (s *Store) AppendNotifications(_ context.Context, account string, items []store.NotificationRecord) (int, error) {
	rows := make([]histKey, 0, len(items))
	payloads := make([]json.RawMessage, 0, len(items))
	for _, it := range items {
		rows = append(rows, histKey{account: account, scope: it.Tab, id: it.NotificationID})
		payloads = append(payloads, it.Payload)
	}
	return s.append(s.notificationsMap, rows, payloads), nil
}

func (s *Store) AppendComments(_ context.Context, account string, items []store.CommentRecord) (int, error) {
	rows := make([]histKey, 0, len(items))
	payloads := make([]json.RawMessage, 0, len(items))
	for _, it := range items {
		rows = append(rows, histKey{account: account, scope: it.NoteID, id: it.CommentID})
		payloads = append(payloads, it.Payload)
	}
	return s.append(s.commentsMap, rows, payloads), nil
}

func (s *Store) notificationsMap() map[histKey]*histRow { return s.notifications }
func (s *Store) commentsMap() map[histKey]*histRow      { return s.comments }

// append inserts the rows that are not already present and returns how many
// were new. An existing row keeps its sequence number, its first-seen time and
// its original payload: a re-fetch must not move a row past a cursor a caller
// has already stored.
func (s *Store) append(pick func() map[histKey]*histRow, keys []histKey, payloads []json.RawMessage) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	m := pick()
	now := normalizeTime(s.now())
	added := 0
	for i, k := range keys {
		if _, ok := m[k]; ok {
			continue
		}
		s.seq++
		m[k] = &histRow{seq: s.seq, payload: cloneJSON(payloads[i]), firstSeen: now}
		added++
	}
	return added
}

func (s *Store) NotificationsSince(_ context.Context, account, tab string, after store.Cursor, limit int) ([]store.HistoryItem, error) {
	return s.since(s.notificationsMap, account, tab, after, limit), nil
}

func (s *Store) CommentsSince(_ context.Context, account, noteID string, after store.Cursor, limit int) ([]store.HistoryItem, error) {
	return s.since(s.commentsMap, account, noteID, after, limit), nil
}

func (s *Store) since(pick func() map[histKey]*histRow, account, scope string, after store.Cursor, limit int) []store.HistoryItem {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := []store.HistoryItem{}
	for k, row := range pick() {
		if k.account != account || k.scope != scope {
			continue
		}
		cursor := formatCursor(row.seq)
		// Cursors are fixed width, so a byte comparison is the same ordering
		// the rows are returned in.
		if after != "" && string(cursor) <= string(after) {
			continue
		}
		out = append(out, store.HistoryItem{
			Cursor:    cursor,
			Payload:   cloneJSON(row.payload),
			FirstSeen: row.firstSeen,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cursor < out[j].Cursor })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (s *Store) UpsertAccount(_ context.Context, a store.Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := normalizeTime(s.now())
	if a.FirstSeen.IsZero() {
		a.FirstSeen = now
	} else {
		a.FirstSeen = normalizeTime(a.FirstSeen)
	}
	if a.LastSeen.IsZero() {
		a.LastSeen = now
	} else {
		a.LastSeen = normalizeTime(a.LastSeen)
	}
	if prev, ok := s.accounts[a.AccountID]; ok {
		// An account is first seen once. Everything else is current state.
		a.FirstSeen = prev.FirstSeen
	}
	s.accounts[a.AccountID] = a
	return nil
}

func (s *Store) AccountBySeed(_ context.Context, seed int) (store.Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var best store.Account
	found := false
	for _, a := range s.accounts {
		if a.Seed != seed {
			continue
		}
		// Ties broken by account id so the answer is deterministic; the
		// contract only promises "most recently seen".
		if !found || a.LastSeen.After(best.LastSeen) ||
			(a.LastSeen.Equal(best.LastSeen) && a.AccountID > best.AccountID) {
			best, found = a, true
		}
	}
	if !found {
		return store.Account{}, store.ErrNotFound
	}
	return best, nil
}

func (s *Store) PruneDocs(_ context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		// Refuse to interpret a missing retention setting as "delete
		// everything".
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := s.now().Add(-olderThan)
	var n int64
	for k, doc := range s.docs {
		if doc.FetchedAt.Before(cutoff) {
			delete(s.docs, k)
			n++
		}
	}
	return n, nil
}

// Close is a no-op; there is nothing to release. It stays callable more than
// once so shutdown paths do not need to track whether they already ran.
func (s *Store) Close() error { return nil }

func (s *Store) clock() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.now()
}
