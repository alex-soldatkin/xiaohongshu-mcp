// Package pgstore is the PostgreSQL backend for store.Store.
//
// It is the default backend when XHS_DATABASE_URL is set. The schema is three
// tables and a bookkeeping one: a document cache keyed by (account, kind, key)
// and two append-only history logs read back by cursor. There is no ORM, no
// query builder and no reflection mapping — every statement is a SQL string
// next to the method that runs it, because there are a dozen of them and they
// never vary.
//
// Correctness of this package is defined by storetest.Run, not by this file.
package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/store"
)

// emptyMeta is what a nil or empty Meta becomes, matching the column default
// so that an accept predicate can unmarshal without a length check first.
var emptyMeta = json.RawMessage(`{}`)

// Store is a store.Store backed by PostgreSQL. Use Open.
type Store struct {
	pool *pgxpool.Pool

	// closeOnce keeps Close idempotent; shutdown paths should not have to
	// track whether they already ran.
	closeOnce sync.Once

	// now is the store clock, filling in zero timestamps the caller left for
	// us. It is a field so tests can age rows without sleeping.
	now func() time.Time
}

// Store implements store.Store.
var _ store.Store = (*Store)(nil)

// Open connects to PostgreSQL, applies the embedded migrations and returns a
// ready store. A URL that parses but does not answer is an error here rather
// than a surprise on the first cache read: the caller treats an unreachable
// database as fatal at startup, on the grounds that an operator who configured
// caching should not be left believing it works.
func Open(ctx context.Context, url string) (*Store, error) {
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("pgstore: invalid database url: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("pgstore: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgstore: ping: %w", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool, now: time.Now}, nil
}

// Pool exposes the connection pool. It is here so that a test can inspect the
// schema it just migrated; nothing in the cache layer should need it.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// SetClock replaces the time source used to fill zero timestamps.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

func (s *Store) Close() error {
	s.closeOnce.Do(s.pool.Close)
	return nil
}

// --- documents -------------------------------------------------------------

const getDocSQL = `
	SELECT payload, meta, fetched_at
	FROM documents
	WHERE account_id = $1 AND kind = $2 AND key = $3`

func (s *Store) GetDoc(ctx context.Context, account string, kind store.Kind, key string) (store.Doc, error) {
	var (
		payload []byte
		meta    []byte
		fetched time.Time
	)
	err := s.pool.QueryRow(ctx, getDocSQL, account, string(kind), key).Scan(&payload, &meta, &fetched)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// A miss returns the zero Doc, never a half-filled one.
		return store.Doc{}, store.ErrNotFound
	case err != nil:
		return store.Doc{}, fmt.Errorf("pgstore: get doc: %w", err)
	}

	if len(meta) == 0 {
		meta = emptyMeta
	}
	return store.Doc{
		Payload:   json.RawMessage(payload),
		Meta:      json.RawMessage(meta),
		FetchedAt: fetched.UTC(),
	}, nil
}

const putDocSQL = `
	INSERT INTO documents (account_id, kind, key, payload, meta, fetched_at)
	VALUES ($1, $2, $3, $4, $5, $6)
	ON CONFLICT (account_id, kind, key) DO UPDATE
	SET payload = EXCLUDED.payload, meta = EXCLUDED.meta, fetched_at = EXCLUDED.fetched_at`

func (s *Store) PutDoc(ctx context.Context, account string, kind store.Kind, key string, doc store.Doc) error {
	meta := doc.Meta
	if len(meta) == 0 {
		meta = emptyMeta
	}
	fetchedAt := doc.FetchedAt
	if fetchedAt.IsZero() {
		fetchedAt = s.now()
	}

	_, err := s.pool.Exec(ctx, putDocSQL,
		account, string(kind), key, []byte(doc.Payload), []byte(meta), fetchedAt)
	if err != nil {
		return fmt.Errorf("pgstore: put doc: %w", err)
	}
	return nil
}

const (
	deleteDocKindSQL = `DELETE FROM documents WHERE account_id = $1 AND kind = $2`
	deleteDocKeysSQL = deleteDocKindSQL + ` AND key = ANY($3::text[])`
)

func (s *Store) DeleteDocs(ctx context.Context, account string, kind store.Kind, keys ...string) error {
	var err error
	if len(keys) == 0 {
		// No keys means the whole kind: one invalidation wipes every cached
		// notification listing without enumerating tab/limit combinations.
		_, err = s.pool.Exec(ctx, deleteDocKindSQL, account, string(kind))
	} else {
		_, err = s.pool.Exec(ctx, deleteDocKeysSQL, account, string(kind), keys)
	}
	if err != nil {
		return fmt.Errorf("pgstore: delete docs: %w", err)
	}
	return nil
}

const pruneDocsSQL = `DELETE FROM documents WHERE fetched_at < $1`

func (s *Store) PruneDocs(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		// A missing or misparsed retention setting arrives here as a zero
		// duration. Reading that as "everything is older than now" would empty
		// the cache, so it is a no-op instead.
		return 0, nil
	}

	tag, err := s.pool.Exec(ctx, pruneDocsSQL, s.now().Add(-olderThan))
	if err != nil {
		return 0, fmt.Errorf("pgstore: prune docs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// --- cursors ---------------------------------------------------------------

// cursorWidth is the zero-padded width of a rendered sequence number. The
// interface requires cursors to sort lexicographically by byte value in the
// order rows are returned, and a bare decimal does not: "10" < "9". A
// bigserial is at most 19 digits, so 20 never truncates.
const cursorWidth = 20

// cursorExpr renders a sequence number as a cursor. It must agree with
// parseCursor and with the padding used anywhere else.
const cursorExpr = `lpad(seq::text, 20, '0')`

// parseCursor turns a cursor back into the sequence number it was rendered
// from. Filtering on the number rather than on the padded text is what lets
// the (account, scope, seq) index do the work.
func parseCursor(c store.Cursor) (int64, error) {
	if c == "" {
		// The start of the stream. Sequences are positive, so 0 excludes
		// nothing.
		return 0, nil
	}
	seq, err := strconv.ParseInt(strings.TrimLeft(string(c), "0"), 10, 64)
	if err != nil {
		if strings.Trim(string(c), "0") == "" {
			return 0, nil
		}
		return 0, fmt.Errorf("pgstore: malformed cursor %q", string(c))
	}
	return seq, nil
}

// --- history ---------------------------------------------------------------

const appendNotificationsSQL = `
	INSERT INTO notifications (account_id, tab, notification_id, from_user_id, note_id, comment_id, payload)
	SELECT $1, t.tab, t.notification_id, t.from_user_id, t.note_id, t.comment_id, t.payload::json
	FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[])
	     WITH ORDINALITY AS t(tab, notification_id, from_user_id, note_id, comment_id, payload, ord)
	ORDER BY t.ord
	ON CONFLICT (account_id, tab, notification_id) DO NOTHING`

func (s *Store) AppendNotifications(ctx context.Context, account string, items []store.NotificationRecord) (int, error) {
	// The site repeats items across an overlapping page boundary, so a batch
	// can contain the same row twice. Collapsing here keeps the returned count
	// honest and keeps the insert order the caller gave us.
	var (
		seen                                                = map[[2]string]bool{}
		tabs, ids, fromUsers, noteIDs, commentIDs, payloads []string
	)
	for _, it := range items {
		k := [2]string{it.Tab, it.NotificationID}
		if seen[k] {
			continue
		}
		seen[k] = true
		tabs = append(tabs, it.Tab)
		ids = append(ids, it.NotificationID)
		fromUsers = append(fromUsers, it.FromUserID)
		noteIDs = append(noteIDs, it.NoteID)
		commentIDs = append(commentIDs, it.CommentID)
		payloads = append(payloads, string(it.Payload))
	}
	if len(ids) == 0 {
		return 0, nil
	}

	tag, err := s.pool.Exec(ctx, appendNotificationsSQL,
		account, tabs, ids, fromUsers, noteIDs, commentIDs, payloads)
	if err != nil {
		return 0, fmt.Errorf("pgstore: append notifications: %w", err)
	}
	// DO NOTHING means the affected count is exactly the number of rows that
	// were genuinely new, which is the "new since last sync" figure the agent
	// is told.
	return int(tag.RowsAffected()), nil
}

const appendCommentsSQL = `
	INSERT INTO comments (account_id, note_id, comment_id, parent_id, author_id, payload)
	SELECT $1, t.note_id, t.comment_id, t.parent_id, t.author_id, t.payload::json
	FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[])
	     WITH ORDINALITY AS t(note_id, comment_id, parent_id, author_id, payload, ord)
	ORDER BY t.ord
	ON CONFLICT (account_id, note_id, comment_id) DO NOTHING`

func (s *Store) AppendComments(ctx context.Context, account string, items []store.CommentRecord) (int, error) {
	var (
		seen                                     = map[[2]string]bool{}
		noteIDs, ids, parents, authors, payloads []string
	)
	for _, it := range items {
		k := [2]string{it.NoteID, it.CommentID}
		if seen[k] {
			continue
		}
		seen[k] = true
		noteIDs = append(noteIDs, it.NoteID)
		ids = append(ids, it.CommentID)
		parents = append(parents, it.ParentID)
		authors = append(authors, it.AuthorID)
		payloads = append(payloads, string(it.Payload))
	}
	if len(ids) == 0 {
		return 0, nil
	}

	tag, err := s.pool.Exec(ctx, appendCommentsSQL, account, noteIDs, ids, parents, authors, payloads)
	if err != nil {
		return 0, fmt.Errorf("pgstore: append comments: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// sinceSQL is the same scan for both logs; only the scope column differs. The
// limit arrives as NULL when it is zero, which PostgreSQL reads as no limit.
func sinceSQL(table, scopeColumn string) string {
	return fmt.Sprintf(`
		SELECT %s, payload, first_seen
		FROM %s
		WHERE account_id = $1 AND %s = $2 AND seq > $3
		ORDER BY seq
		LIMIT nullif($4::int, 0)`, cursorExpr, table, scopeColumn)
}

var (
	notificationsSinceSQL = sinceSQL("notifications", "tab")
	commentsSinceSQL      = sinceSQL("comments", "note_id")
)

func (s *Store) NotificationsSince(ctx context.Context, account, tab string, after store.Cursor, limit int) ([]store.HistoryItem, error) {
	return s.since(ctx, notificationsSinceSQL, account, tab, after, limit)
}

func (s *Store) CommentsSince(ctx context.Context, account, noteID string, after store.Cursor, limit int) ([]store.HistoryItem, error) {
	return s.since(ctx, commentsSinceSQL, account, noteID, after, limit)
}

func (s *Store) since(ctx context.Context, query, account, scope string, after store.Cursor, limit int) ([]store.HistoryItem, error) {
	seq, err := parseCursor(after)
	if err != nil {
		return nil, err
	}
	if limit < 0 {
		limit = 0
	}

	rows, err := s.pool.Query(ctx, query, account, scope, seq, limit)
	if err != nil {
		return nil, fmt.Errorf("pgstore: history scan: %w", err)
	}
	defer rows.Close()

	// An empty stream is an empty slice and a nil error, never a nil slice
	// dressed up as an error.
	out := []store.HistoryItem{}
	for rows.Next() {
		var (
			cursor    string
			payload   []byte
			firstSeen time.Time
		)
		if err := rows.Scan(&cursor, &payload, &firstSeen); err != nil {
			return nil, fmt.Errorf("pgstore: history scan: %w", err)
		}
		out = append(out, store.HistoryItem{
			Cursor:    store.Cursor(cursor),
			Payload:   json.RawMessage(payload),
			FirstSeen: firstSeen.UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgstore: history scan: %w", err)
	}
	return out, nil
}

// --- accounts --------------------------------------------------------------

const upsertAccountSQL = `
	INSERT INTO accounts (account_id, seed, nickname, first_seen, last_seen)
	VALUES ($1, $2, $3, $4, $5)
	ON CONFLICT (account_id) DO UPDATE
	SET seed = EXCLUDED.seed, nickname = EXCLUDED.nickname, last_seen = EXCLUDED.last_seen`

func (s *Store) UpsertAccount(ctx context.Context, a store.Account) error {
	now := s.now()
	if a.FirstSeen.IsZero() {
		a.FirstSeen = now
	}
	if a.LastSeen.IsZero() {
		a.LastSeen = now
	}

	// first_seen is absent from the DO UPDATE list on purpose: an account is
	// first seen once, and an upsert records current state rather than
	// rewriting history.
	_, err := s.pool.Exec(ctx, upsertAccountSQL,
		a.AccountID, int64(a.Seed), a.Nickname, a.FirstSeen, a.LastSeen)
	if err != nil {
		return fmt.Errorf("pgstore: upsert account: %w", err)
	}
	return nil
}

const accountBySeedSQL = `
	SELECT account_id, seed, nickname, first_seen, last_seen
	FROM accounts
	WHERE seed = $1
	ORDER BY last_seen DESC, account_id DESC
	LIMIT 1`

func (s *Store) AccountBySeed(ctx context.Context, seed int) (store.Account, error) {
	var (
		a       store.Account
		seedOut int64
	)
	err := s.pool.QueryRow(ctx, accountBySeedSQL, int64(seed)).
		Scan(&a.AccountID, &seedOut, &a.Nickname, &a.FirstSeen, &a.LastSeen)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Only the account currently on this seed answers for it: #6 keeps one
		// seed across a re-login, so the mapping is many-to-one over time.
		return store.Account{}, store.ErrNotFound
	case err != nil:
		return store.Account{}, fmt.Errorf("pgstore: account by seed: %w", err)
	}

	a.Seed = int(seedOut)
	a.FirstSeen = a.FirstSeen.UTC()
	a.LastSeen = a.LastSeen.UTC()
	return a, nil
}
