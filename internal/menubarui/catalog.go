package menubarui

import (
	"context"
	"time"
)

// FetchCatalog loads the model catalog with a bounded timeout and stores it
// in the cache. Callers run it on a background goroutine.
func FetchCatalog(ctx context.Context, ctrl Controller, cache *CatalogCache, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	choices, err := ctrl.Catalog(ctx)
	if err != nil {
		return err
	}
	cache.Store(choices)
	return nil
}
