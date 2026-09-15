package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/configs"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/pacing"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/store"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// This file is the read-through cache for issue #7 (WS2).
//
// The single structural rule, and the reason the seam is here rather than
// inside XiaohongshuService.run: the cache is consulted BEFORE the pacing gate
// is acquired. Issue #5 made every gate acquisition pay a 3-8s lognormal gap
// and record a read against the account's hourly budget. A cache hit routed
// through the gate would still cost seconds and still spend budget, which is
// the entire thing this feature exists to avoid. Nothing below may move inside
// run.
//
// The second rule: with the store unset (a store.Nop), the cost of all of this
// is one boolean test. Nothing is marshalled, no store method is called, and
// the response bytes are identical to what the server returned before WS2.

// Default TTLs, per the plan's section 3 table. Note details and profiles are
// near-static; listings and counters are volatile. A TTL of zero disables
// caching for that kind, matching pacing's convention that zero means off.
const (
	defaultTTLNote          = 6 * time.Hour
	defaultTTLProfile       = 6 * time.Hour
	defaultTTLMyProfile     = 30 * time.Minute
	defaultTTLFeed          = 5 * time.Minute
	defaultTTLSearch        = 15 * time.Minute
	defaultTTLNotifications = 5 * time.Minute
	defaultTTLUnread        = 2 * time.Minute

	// defaultCacheRetention is how long a document may sit in the store before
	// the retention sweep removes it, regardless of TTL. A stale document is
	// never served — TTL already rules it out — so this is about the size of
	// the table, not correctness.
	defaultCacheRetention = 720 * time.Hour

	// cachePruneInterval is how often the retention sweep runs after startup.
	cachePruneInterval = time.Hour
)

// cacheTTLEnv maps each kind to the environment variable that overrides its
// TTL. note and note_full deliberately share one variable: they are the same
// data at two levels of completeness and tuning them apart would be a trap.
var cacheTTLEnv = map[store.Kind]string{
	store.KindNote:          "XHS_CACHE_TTL_NOTE",
	store.KindNoteFull:      "XHS_CACHE_TTL_NOTE",
	store.KindProfile:       "XHS_CACHE_TTL_PROFILE",
	store.KindMyProfile:     "XHS_CACHE_TTL_MY_PROFILE",
	store.KindFeed:          "XHS_CACHE_TTL_FEED",
	store.KindSearch:        "XHS_CACHE_TTL_SEARCH",
	store.KindNotifications: "XHS_CACHE_TTL_NOTIFICATIONS",
	store.KindUnread:        "XHS_CACHE_TTL_UNREAD",
}

// defaultCacheTTLs returns the built-in TTL table.
func defaultCacheTTLs() map[store.Kind]time.Duration {
	return map[store.Kind]time.Duration{
		store.KindNote:          defaultTTLNote,
		store.KindNoteFull:      defaultTTLNote,
		store.KindProfile:       defaultTTLProfile,
		store.KindMyProfile:     defaultTTLMyProfile,
		store.KindFeed:          defaultTTLFeed,
		store.KindSearch:        defaultTTLSearch,
		store.KindNotifications: defaultTTLNotifications,
		store.KindUnread:        defaultTTLUnread,
	}
}

// cacheConfig is the TTL table plus the retention window, after env overrides.
type cacheConfig struct {
	TTLs      map[store.Kind]time.Duration
	Retention time.Duration
}

// cacheConfigFromEnv starts from the defaults and applies the XHS_CACHE_*
// overrides. An explicit zero is honoured and disables that kind.
func cacheConfigFromEnv() cacheConfig {
	cfg := cacheConfig{TTLs: defaultCacheTTLs(), Retention: defaultCacheRetention}

	for kind, key := range cacheTTLEnv {
		if d, ok := cacheEnvDuration(key); ok {
			cfg.TTLs[kind] = d
		}
	}
	if d, ok := cacheEnvDuration("XHS_CACHE_RETENTION"); ok {
		cfg.Retention = d
	}
	return cfg
}

// cacheEnvDuration accepts a Go duration ("30m") or a bare number of seconds,
// the same shapes pacing's knobs take. Zero is a legitimate value, so the
// second return distinguishes unset from set-to-zero.
func cacheEnvDuration(key string) (time.Duration, bool) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, false
	}
	if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
		return d, true
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	logrus.Warnf("invalid %s=%q, ignored", key, raw)
	return 0, false
}

// serviceCache holds everything the read-through layer needs: the store, the
// TTL table, the clock (injectable so tests can age documents without
// sleeping) and the account the documents are scoped to.
type serviceCache struct {
	store   store.Store
	enabled bool
	cfg     cacheConfig

	// now is the clock. Tests replace it; production never does.
	now func() time.Time

	// account is the XHS user id every document is keyed under. It is a
	// pointer because "not yet known" is a real state: on a fresh database
	// nothing is known until the first successful read observes the id from
	// the page. Deliberately not the fingerprint seed — the seed survives a
	// logout and re-login (issue #6, D1), so it names a device, not an
	// account, and scoping by it would merge two accounts' data.
	account atomic.Pointer[string]

	stop     chan struct{}
	done     chan struct{}
	started  bool
	stopOnce sync.Once
}

// newServiceCache wires a store into a cache layer.
//
// A nil store or a store.Nop means disabled: every helper below short-circuits
// on the enabled flag before touching anything, so the default deployment
// (XHS_DATABASE_URL unset) pays one boolean test per read.
func newServiceCache(st store.Store) *serviceCache {
	c := &serviceCache{
		store: st,
		cfg:   cacheConfigFromEnv(),
		now:   time.Now,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	if st == nil {
		c.store = store.Nop{}
		return c
	}
	if _, isNop := st.(store.Nop); isNop {
		return c
	}
	c.enabled = true
	return c
}

// seedAccount recovers the account this deployment last drove under the
// current fingerprint seed, so a restart costs no browser trip to find out who
// we are. Failure is normal on a fresh database and leaves the account unknown.
func (c *serviceCache) seedAccount(ctx context.Context) {
	if !c.enabled {
		return
	}
	seed := configs.FingerprintSeed()
	if seed <= 0 {
		return
	}
	acc, err := c.store.AccountBySeed(ctx, seed)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			logrus.Warnf("cache: account lookup by seed failed: %v", err)
		}
		return
	}
	if acc.AccountID != "" {
		c.setAccount(acc.AccountID)
		logrus.Infof("cache: scoped to account %s (recovered from seed)", acc.AccountID)
	}
}

// accountID returns the current account, or "" when it is not known yet.
func (c *serviceCache) accountID() string {
	if p := c.account.Load(); p != nil {
		return *p
	}
	return ""
}

func (c *serviceCache) setAccount(id string) {
	if id == "" {
		c.account.Store(nil)
		return
	}
	c.account.Store(&id)
}

// clearAccount forgets who we are. Called on a login reset: the next read
// observes the new account from the page. Documents already stored stay
// correct precisely because they are keyed by user id rather than by seed.
func (c *serviceCache) clearAccount() {
	c.account.Store(nil)
}

// rememberAccount records an observed account, both in memory and in the
// store, so the next restart can recover it from the seed.
func (c *serviceCache) rememberAccount(ctx context.Context, userID, nickname string) {
	if !c.enabled || userID == "" {
		return
	}
	previous := c.accountID()
	c.setAccount(userID)
	if err := c.store.UpsertAccount(ctx, store.Account{
		AccountID: userID,
		Seed:      configs.FingerprintSeed(),
		Nickname:  nickname,
	}); err != nil {
		logrus.Warnf("cache: recording account %s failed: %v", userID, err)
	}
	if previous != userID {
		logrus.Infof("cache: scoped to account %s", userID)
	}
}

// observeAccount reads the logged-in user out of the page that just served a
// read. CurrentUser only evaluates __INITIAL_STATE__.user.userInfo, which every
// page carries, so this costs no navigation and no extra budget.
//
// Failing is the not-logged-in case: the account stays unknown, every lookup
// misses and nothing is written. That is the correct outcome — a guest view has
// none of the viewer's liked/collected flags and must not be cached under
// anyone.
func (c *serviceCache) observeAccount(ctx context.Context, page *rod.Page) {
	if !c.enabled || c.accountID() != "" || page == nil {
		return
	}
	user, err := xiaohongshu.NewLogin(page).CurrentUser(ctx)
	if err != nil {
		logrus.Debugf("cache: account not observable from page: %v", err)
		return
	}
	c.rememberAccount(ctx, user.UserID, user.Nickname)
}

// ttl returns the configured TTL for a kind. An unknown kind is not cached.
func (c *serviceCache) ttl(kind store.Kind) time.Duration {
	return c.cfg.TTLs[kind]
}

// readable reports whether a lookup is worth attempting at all: the store has
// to be on, the caller must not have asked for live data, and we have to know
// whose data it is.
func (c *serviceCache) readable(ctx context.Context) bool {
	return c.enabled && !forceRefresh(ctx) && c.accountID() != ""
}

// put stores a freshly fetched payload. Store failures are logged and
// swallowed: a cache that cannot write is slow, not broken, and must never
// turn a successful read into an error for the user.
// The TTL consulted here is the configured one for the kind, not whatever the
// caller passed to readThrough; the two are the same at every call site, and
// taking it from the config means a kind switched off by env is neither read
// nor written.
func (c *serviceCache) put(ctx context.Context, kind store.Kind, key string, payload, meta any, fetchedAt time.Time) {
	if !c.enabled || c.ttl(kind) <= 0 {
		return
	}
	account := c.accountID()
	if account == "" {
		return
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		logrus.Warnf("cache: marshalling %s/%s failed: %v", kind, key, err)
		return
	}
	var rawMeta json.RawMessage
	if meta != nil {
		if rawMeta, err = json.Marshal(meta); err != nil {
			logrus.Warnf("cache: marshalling meta for %s/%s failed: %v", kind, key, err)
			return
		}
	}

	if err := c.store.PutDoc(ctx, account, kind, key, store.Doc{
		Payload:   raw,
		Meta:      rawMeta,
		FetchedAt: fetchedAt,
	}); err != nil {
		logrus.Warnf("cache: storing %s/%s failed: %v", kind, key, err)
	}
}

// invalidate drops documents of one kind. With no keys it drops every document
// of that kind for the current account.
//
// Writes call this BEFORE the browser action, not after: an action that fails
// halfway may still have changed the site, and a stale document that survives
// a failed like is worse than a needless refetch. There is no repopulation
// race, because any concurrent read has to wait for the gate slot the write is
// holding and therefore fetches post-write state.
func (c *serviceCache) invalidate(ctx context.Context, kind store.Kind, keys ...string) {
	if !c.enabled {
		return
	}
	account := c.accountID()
	if account == "" {
		return
	}
	if err := c.store.DeleteDocs(ctx, account, kind, keys...); err != nil {
		logrus.Warnf("cache: invalidating %s failed: %v", kind, err)
	}
}

// invalidateNote drops both cached levels of one note. A like changes the
// note's own interaction flags, and note/note_full hold the same note at two
// levels of comment completeness, so neither may survive alone.
func (c *serviceCache) invalidateNote(ctx context.Context, feedID string) {
	if !c.enabled || feedID == "" {
		return
	}
	c.invalidate(ctx, store.KindNote, feedID)
	c.invalidate(ctx, store.KindNoteFull, feedID)
}

// startRetention prunes documents older than the retention window at startup
// and hourly thereafter. History rows are never pruned: a cached page can be
// refetched, a notification that has scrolled off the site cannot.
func (c *serviceCache) startRetention() {
	c.started = true
	if !c.enabled || c.cfg.Retention <= 0 {
		close(c.done)
		return
	}

	go func() {
		defer close(c.done)

		ticker := time.NewTicker(cachePruneInterval)
		defer ticker.Stop()

		c.prune()
		for {
			select {
			case <-c.stop:
				return
			case <-ticker.C:
				c.prune()
			}
		}
	}()
}

func (c *serviceCache) prune() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	n, err := c.store.PruneDocs(ctx, c.cfg.Retention)
	if err != nil {
		logrus.Warnf("cache: retention sweep failed: %v", err)
		return
	}
	if n > 0 {
		logrus.Infof("cache: retention sweep removed %d documents older than %s", n, c.cfg.Retention)
	}
}

// close stops the retention goroutine. The store itself is closed by whoever
// opened it, which is main.
func (c *serviceCache) close() {
	c.stopOnce.Do(func() { close(c.stop) })
	if !c.started {
		// Nothing was ever started, so there is nothing to wait for.
		return
	}
	<-c.done
}

// ---------------------------------------------------------------------------
// force_refresh
// ---------------------------------------------------------------------------

type forceRefreshKey struct{}

// withForceRefresh marks a request as wanting live data.
//
// A flag carried in a context is a known smell. It earns its place here: it has
// exactly one consumer (readThrough), it is request-scoped in the strictest
// sense, and the alternative is a bool threaded through roughly twenty
// signatures across three files for the sole benefit of one if statement.
func withForceRefresh(ctx context.Context, force bool) context.Context {
	if !force {
		return ctx
	}
	return context.WithValue(ctx, forceRefreshKey{}, true)
}

// forceRefresh reports whether this request asked to bypass the cache.
func forceRefresh(ctx context.Context) bool {
	force, _ := ctx.Value(forceRefreshKey{}).(bool)
	return force
}

// ---------------------------------------------------------------------------
// the read-through helper
// ---------------------------------------------------------------------------

// cacheLookup is one (kind, key) pair a request may be served from, with the
// freshness window and the acceptance test that apply to it.
//
// A request usually has exactly one. get_feed_detail has two, because a
// note_full document also satisfies a request for note.
type cacheLookup struct {
	kind   store.Kind
	key    string
	ttl    time.Duration
	accept func(meta json.RawMessage) bool
}

// readThrough serves a read from the cache when it can and from the browser
// when it cannot.
//
// The order is the whole point:
//
//  1. Cache enabled, not force-refreshed, TTL positive, account known, document
//     present, fresh and accepted -> unmarshal and return. No gate, no browser,
//     no pacing budget spent.
//  2. Otherwise s.run with pacing.ClassRead — gap, budget event, browser page —
//     wrapped so that a still-unknown account is observed from the same page
//     once the fetch has succeeded.
//  3. PutDoc, best effort.
//
// fetchedAt is zero when the store is disabled, which is how the callers know
// to omit the cached/fetched_at response fields entirely.
func readThrough[T any](
	ctx context.Context, s *XiaohongshuService,
	kind store.Kind, key string, ttl time.Duration,
	accept func(meta json.RawMessage) bool, meta any,
	fetch func(page *rod.Page) (*T, error),
) (result *T, fetchedAt time.Time, cached bool, err error) {
	return readThroughFrom(ctx, s,
		[]cacheLookup{{kind: kind, key: key, ttl: ttl, accept: accept}},
		kind, key, meta, fetch)
}

// readThroughFrom is readThrough with more than one candidate document. The
// lookups are tried in order; the fetched payload is always written back under
// (putKind, putKey).
func readThroughFrom[T any](
	ctx context.Context, s *XiaohongshuService,
	lookups []cacheLookup, putKind store.Kind, putKey string, meta any,
	fetch func(page *rod.Page) (*T, error),
) (result *T, fetchedAt time.Time, cached bool, err error) {
	c := s.cache

	if c.readable(ctx) {
		account := c.accountID()
		for _, l := range lookups {
			if l.ttl <= 0 {
				continue
			}
			hit, at, ok := lookupDoc[T](ctx, c, account, l)
			if ok {
				logrus.Debugf("cache: hit %s/%s fetched %s ago", l.kind, l.key, c.now().Sub(at).Truncate(time.Second))
				return hit, at, true, nil
			}
		}
	}

	var fetched *T
	if err := s.run(ctx, pacing.ClassRead, func(page *rod.Page) error {
		v, err := fetch(page)
		if err != nil {
			return err
		}
		fetched = v
		// Observe who we are from the page that just worked. Only ever costs
		// an Eval, and only while the account is still unknown.
		c.observeAccount(ctx, page)
		return nil
	}); err != nil {
		return nil, time.Time{}, false, err
	}

	if !c.enabled {
		return fetched, time.Time{}, false, nil
	}

	now := c.now()
	c.put(ctx, putKind, putKey, fetched, meta, now)
	return fetched, now, false, nil
}

// lookupDoc fetches, ages and decodes one candidate document.
func lookupDoc[T any](ctx context.Context, c *serviceCache, account string, l cacheLookup) (*T, time.Time, bool) {
	doc, err := c.store.GetDoc(ctx, account, l.kind, l.key)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			logrus.Warnf("cache: reading %s/%s failed: %v", l.kind, l.key, err)
		}
		return nil, time.Time{}, false
	}
	if c.now().Sub(doc.FetchedAt) > l.ttl {
		return nil, time.Time{}, false
	}
	if l.accept != nil && !l.accept(doc.Meta) {
		return nil, time.Time{}, false
	}

	var v T
	if err := json.Unmarshal(doc.Payload, &v); err != nil {
		// A payload that no longer parses means the struct changed shape under
		// the stored document. Treat it as a miss; the refetch overwrites it.
		logrus.Warnf("cache: stored %s/%s no longer decodes, refetching: %v", l.kind, l.key, err)
		return nil, time.Time{}, false
	}
	return &v, doc.FetchedAt, true
}

// ---------------------------------------------------------------------------
// keys and acceptance predicates
// ---------------------------------------------------------------------------

// searchCacheKey hashes a query into a key. The raw keyword is not used
// directly because it is user text of unbounded length and arbitrary content,
// and the key is part of a primary key.
func searchCacheKey(keyword string, filters ...xiaohongshu.FilterOption) string {
	parts := []string{keyword}
	for _, f := range filters {
		parts = append(parts, f.SortBy, f.NoteType, f.PublishTime, f.SearchScope, f.Location)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])[:16]
}

// profileCacheKey keys another user's profile by user and tab; the tabs are
// separate pages with separate contents.
func profileCacheKey(userID string, tab xiaohongshu.ProfileTab) string {
	return fmt.Sprintf("%s:%s", userID, tab)
}

// commentConfigMeta is what a note_full document records about how completely
// its comments were loaded. Without it a note fetched with MaxCommentItems=20
// would silently satisfy a request for 100.
type commentConfigMeta struct {
	MaxCommentItems     int  `json:"max_comment_items"`
	MaxRepliesThreshold int  `json:"max_replies_threshold"`
	ClickMoreReplies    bool `json:"click_more_replies"`
}

func newCommentConfigMeta(cfg xiaohongshu.CommentLoadConfig) commentConfigMeta {
	return commentConfigMeta{
		MaxCommentItems:     cfg.MaxCommentItems,
		MaxRepliesThreshold: cfg.MaxRepliesThreshold,
		ClickMoreReplies:    cfg.ClickMoreReplies,
	}
}

// acceptCommentConfig accepts a cached note_full only if it was loaded at
// least as completely as this request asks for.
//
// Zero never means "unlimited" here: CommentLoadConfig.normalize replaces a
// zero with the default, so a stored zero is a document written before the
// meta existed and is simply not good enough for a request that states a
// number.
func acceptCommentConfig(want xiaohongshu.CommentLoadConfig) func(json.RawMessage) bool {
	wanted := newCommentConfigMeta(want)

	return func(raw json.RawMessage) bool {
		var have commentConfigMeta
		if len(raw) == 0 || json.Unmarshal(raw, &have) != nil {
			return false
		}
		if have.MaxCommentItems < wanted.MaxCommentItems {
			return false
		}
		if wanted.ClickMoreReplies {
			// Sub-replies were asked for: the document must have expanded them,
			// and must not have skipped comments this request would have kept.
			// A higher threshold skips fewer, so more is more complete.
			if !have.ClickMoreReplies || have.MaxRepliesThreshold < wanted.MaxRepliesThreshold {
				return false
			}
		}
		return true
	}
}

// notificationListMeta records how many items a cached listing was fetched
// with, so a listing of 50 can serve a request for 20.
type notificationListMeta struct {
	Limit int `json:"limit"`
}

// acceptNotificationLimit accepts a cached listing that is at least as long as
// the one being asked for.
//
// This is why the document is keyed by tab alone and not by "<tab>:<limit>" as
// the plan's table had it: with the limit in the key, every distinct limit is
// its own document and the acceptance rule the plan asks for in section 5 can
// never fire. Keying by tab and testing the limit in Go is the version that
// actually implements the stated behaviour.
func acceptNotificationLimit(want int) func(json.RawMessage) bool {
	return func(raw json.RawMessage) bool {
		var have notificationListMeta
		if len(raw) == 0 || json.Unmarshal(raw, &have) != nil {
			return false
		}
		if want <= 0 {
			// No limit requested: only an unlimited listing will do.
			return have.Limit <= 0
		}
		return have.Limit <= 0 || have.Limit >= want
	}
}
