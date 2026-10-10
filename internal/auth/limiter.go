package auth

import (
	"sync"
	"time"
)

// sendLimiter bounds how many sign-in mails one address, and the whole
// service, may cause in a window. The sign-in routes take no session, so
// without it anyone who knows an address can fill that inbox and spend the
// mailer's quota. Counts are in memory: a restart clears them, which is
// acceptable for a brake, not a ledger.
type sendLimiter struct {
	mu        sync.Mutex
	perKey    int
	total     int
	window    time.Duration
	now       func() time.Time
	byKey     map[string]*keyHits
	all       []time.Time
	maxKeys   int
	lastSweep time.Time
}

// keyHits are one key's recent attempts and whether its current refusal
// streak has been reported already.
type keyHits struct {
	times  []time.Time
	warned bool
}

func newSendLimiter(perKey, total int, window time.Duration) *sendLimiter {
	return &sendLimiter{perKey: perKey, total: total, window: window, now: time.Now, byKey: map[string]*keyHits{}, maxKeys: 10000}
}

// Allow records an attempt for key and reports whether it is within both
// limits. A refused attempt is not recorded, so a flood does not keep the
// window shut for the real owner longer than the window itself.
func (l *sendLimiter) Allow(key string) bool {
	ok, _ := l.AllowReport(key)
	return ok
}

// AllowReport is Allow, and also says whether this refusal is the first of
// a streak for the key, so the caller can log once per streak instead of
// once per request. The streak ends when the key is allowed again.
func (l *sendLimiter) AllowReport(key string) (allowed, firstRefusal bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-l.window)
	l.all = prune(l.all, cutoff)
	k := l.byKey[key]
	if k == nil {
		k = &keyHits{}
		l.byKey[key] = k
	}
	k.times = prune(k.times, cutoff)
	if len(l.all) >= l.total || len(k.times) >= l.perKey {
		first := !k.warned
		k.warned = true
		return false, first
	}
	k.warned = false
	k.times = append(k.times, now)
	l.all = append(l.all, now)
	if len(l.byKey) > l.maxKeys || now.Sub(l.lastSweep) > l.window {
		l.sweep(cutoff)
		l.lastSweep = now
	}
	return true, false
}

func (l *sendLimiter) sweep(cutoff time.Time) {
	for key, k := range l.byKey {
		if k.times = prune(k.times, cutoff); len(k.times) == 0 && !k.warned {
			delete(l.byKey, key)
		}
	}
}

// prune drops times at or before cutoff from the front of a sorted slice.
func prune(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(cutoff) {
		i++
	}
	return ts[i:]
}
