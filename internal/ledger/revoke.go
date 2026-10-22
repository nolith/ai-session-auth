package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// A Revoker undoes the entries one provider created; every provider is one.
type Revoker interface {
	Revoke(ctx context.Context, e Entry) error
}

var (
	// ErrGone is a Revoke that found nothing left to revoke: the credential
	// was revoked already, or expired.
	ErrGone = errors.New("already revoked or expired")
	// ErrNotFound is a Revoke that found no credential with the entry's name.
	// Either the session died before the create reached the provider, or the
	// create is still on its way: only the caller knows which.
	ErrNotFound = errors.New("no credential has this name")
	// ErrNoProvider is an entry whose provider is not configured any more.
	ErrNoProvider = errors.New("no such provider in the configuration")
)

// Result is what revoking one entry came to: Err is nil once it is revoked.
type Result struct {
	Entry Entry
	Err   error
}

// Revoke revokes entries through their providers, in parallel across
// providers and one at a time within each, since a provider's requests share
// its issuer. An entry is recorded as revoked once its provider revoked it or
// found nothing left to revoke. The results are in the order of entries.
func (l *Ledger) Revoke(ctx context.Context, entries []Entry, providers map[string]Revoker) []Result {
	results := make([]Result, len(entries))
	byProvider := map[string][]int{}
	for i, e := range entries {
		results[i].Entry = e
		byProvider[e.Provider] = append(byProvider[e.Provider], i)
	}
	var wg sync.WaitGroup
	for id, indexes := range byProvider {
		revoker, ok := providers[id]
		if !ok {
			for _, i := range indexes {
				results[i].Err = fmt.Errorf("%w: %s", ErrNoProvider, id)
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, i := range indexes {
				e := entries[i]
				err := revoker.Revoke(ctx, e)
				if err == nil || errors.Is(err, ErrGone) {
					e.Revoked = true
					if recordErr := l.Record(e); recordErr != nil {
						err = recordErr
					}
				}
				results[i].Err = err
			}
		}()
	}
	wg.Wait()
	return results
}
