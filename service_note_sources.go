package main

import (
	"context"
	"errors"

	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/store"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// This file is WS4 of issue #7: note provenance in the store.
//
// The xiaohongshu package records which surface handed out each note's
// xsec_token, so that opening the note later declares a source that agrees
// with the Referer it sends. That table used to live only in memory, which
// meant a restart silently downgraded every note to the "came from the feed"
// default while the cached listing it actually came from was still being
// served from the store. Persisting it puts the two back in step.
//
// The seam is xiaohongshu.NoteSources; the store side is store.NoteSourceStore,
// an optional capability. A backend that does not implement it, or a
// deployment with no database at all, keeps the in-memory table and behaves
// exactly as before.

// storeNoteSources is the store-backed xiaohongshu.NoteSources.
//
// It keeps an in-memory table alongside the store rather than replacing it,
// for a reason that only shows up in the ordering: provenance is recorded by
// the very reads that first observe the account id, and the recording happens
// inside the browser action while the observation happens after it. So on a
// fresh database the first listing of a session is remembered before anyone
// knows whose listing it is. The local table covers exactly that window, and
// costs one bounded map either way.
type storeNoteSources struct {
	cache *serviceCache
	store store.NoteSourceStore
	local xiaohongshu.NoteSources
}

// newStoreNoteSources returns the store-backed implementation, or nil when the
// store is absent or does not offer the capability — in which case the caller
// leaves the xiaohongshu package on its own default.
func newStoreNoteSources(c *serviceCache) xiaohongshu.NoteSources {
	if c == nil || !c.enabled || c.noteSources == nil {
		return nil
	}
	return &storeNoteSources{
		cache: c,
		store: c.noteSources,
		local: xiaohongshu.NewMemoryNoteSources(),
	}
}

// Remember records where a token came from. The local table always gets it;
// the store gets it too once we know which account the record belongs to.
func (s *storeNoteSources) Remember(ctx context.Context, feedID, source, referrer string) {
	s.local.Remember(ctx, feedID, source, referrer)

	account := s.cache.accountID()
	if account == "" || feedID == "" || source == "" {
		return
	}
	if err := s.store.RememberNoteSource(ctx, account, store.NoteSource{
		FeedID:   feedID,
		Source:   source,
		Referrer: referrer,
	}); err != nil {
		// Provenance is advisory: a failure here costs a fallback claim, not
		// a failed read.
		logrus.Warnf("note source: recording %s failed: %v", feedID, err)
	}
}

// Lookup asks the store first, because it is the copy that survives a restart,
// and falls back to the local table for records written before the account was
// known. The freshness window is applied by the store's own query, so a record
// past it never reaches this code.
func (s *storeNoteSources) Lookup(ctx context.Context, feedID string) (string, string, bool) {
	if account := s.cache.accountID(); account != "" && feedID != "" {
		src, err := s.store.LookupNoteSource(ctx, account, feedID, xiaohongshu.NoteSourceTTL)
		switch {
		case err == nil:
			return src.Source, src.Referrer, true
		case !errors.Is(err, store.ErrNotFound):
			logrus.Warnf("note source: reading %s failed: %v", feedID, err)
		}
	}
	return s.local.Lookup(ctx, feedID)
}
