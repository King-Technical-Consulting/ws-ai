package store

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/sealed"
)

// KeyStore answers the gateway's question "which hosted providers may this
// person use, and with whose key" (gateway.KeySource). The owner, and anyone
// the owner granted shared_provider_keys, may use the server's keys; everyone
// may use keys they saved themselves. The result is cached for a few seconds
// (not the keys: those are opened from their sealed form on use), and
// Invalidate drops a person's entry when they save, remove or are granted a
// key, so the change is felt at once.
type KeyStore struct {
	DB  *DB
	Box *sealed.Box // nil: nobody has saved keys

	mu    sync.Mutex
	cache map[string]cachedAccess
}

type cachedAccess struct {
	shared bool
	sealed map[string][]byte
	at     time.Time
}

const keyAccessTTL = 15 * time.Second

// Invalidate forgets what is cached for a user.
func (k *KeyStore) Invalidate(userID string) {
	k.mu.Lock()
	delete(k.cache, userID)
	k.mu.Unlock()
}

// Access implements gateway.KeySource.
func (k *KeyStore) Access(ctx context.Context, userID string) (*gateway.KeyAccess, error) {
	k.mu.Lock()
	c, ok := k.cache[userID]
	k.mu.Unlock()
	if !ok || time.Since(c.at) > keyAccessTTL {
		id, err := uuid.Parse(userID)
		if err != nil {
			return nil, err
		}
		u, err := k.DB.GetUserByID(ctx, id)
		if err != nil {
			return nil, err
		}
		rows, err := k.DB.ListUserProviderKeysSealed(ctx, id)
		if err != nil {
			return nil, err
		}
		c = cachedAccess{shared: u.Role == "owner" || u.SharedProviderKeys, sealed: map[string][]byte{}, at: time.Now()}
		for _, r := range rows {
			c.sealed[r.ProviderID] = r.Sealed
		}
		k.mu.Lock()
		if k.cache == nil {
			k.cache = map[string]cachedAccess{}
		}
		k.cache[userID] = c
		k.mu.Unlock()
	}
	acc := &gateway.KeyAccess{Shared: c.shared, Own: map[string]bool{}}
	if k.Box == nil {
		return acc, nil
	}
	for p := range c.sealed {
		acc.Own[p] = true
	}
	acc.Open = func(providerID string) (string, bool) {
		blob, ok := c.sealed[providerID]
		if !ok {
			return "", false
		}
		plain, err := k.Box.Open(blob, sealed.ProviderKeyAAD(userID, providerID))
		if err != nil {
			return "", false
		}
		return string(plain), true
	}
	return acc, nil
}
