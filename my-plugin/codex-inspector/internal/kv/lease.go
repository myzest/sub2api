package kv

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"time"
)

// Lease is best effort: the host API has no compare-and-swap/SET-NX.
// Read-back and fencing checks reduce collisions but cannot guarantee exclusivity.
type Lease struct {
	KV      KV
	NS, Key string
	Jitter  func() time.Duration
}

func (l Lease) Acquire(ctx context.Context, holder string, ttl time.Duration) (bool, error) {
	if l.KV == nil || holder == "" || ttl <= 0 {
		return false, errors.New("invalid lease")
	}
	value, found, err := l.KV.Get(ctx, l.NS, l.Key)
	if err != nil {
		return false, err
	}
	if found && string(value) != holder {
		return false, nil
	}
	if !found {
		var delay time.Duration
		if l.Jitter != nil {
			delay = l.Jitter()
		} else {
			n, e := rand.Int(rand.Reader, big.NewInt(501))
			if e != nil {
				return false, e
			}
			delay = time.Duration(n.Int64()) * time.Millisecond
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-timer.C:
			}
		}
		value, found, err = l.KV.Get(ctx, l.NS, l.Key)
		if err != nil {
			return false, err
		}
		if found && string(value) != holder {
			return false, nil
		}
	}
	if err = l.KV.Set(ctx, l.NS, l.Key, []byte(holder), ttl); err != nil {
		return false, err
	}
	return l.Holds(ctx, holder)
}
func (l Lease) Holds(ctx context.Context, holder string) (bool, error) {
	v, ok, e := l.KV.Get(ctx, l.NS, l.Key)
	return ok && string(v) == holder, e
}
