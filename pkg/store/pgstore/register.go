package pgstore

import (
	"context"

	"github.com/xpzouying/xiaohongshu-mcp/pkg/store"
)

// Linking this package teaches store.Open about PostgreSQL URLs. Both spellings
// are registered because both are accepted by libpq and by every connection
// string a user is likely to copy.
func init() {
	store.Register("postgres", openStore)
	store.Register("postgresql", openStore)
}

// openStore adapts Open to store.Opener. The explicit nil on the error path
// matters: returning a typed nil *Store would give the caller a non-nil
// interface holding nothing.
func openStore(ctx context.Context, url string) (store.Store, error) {
	s, err := Open(ctx, url)
	if err != nil {
		return nil, err
	}
	return s, nil
}
