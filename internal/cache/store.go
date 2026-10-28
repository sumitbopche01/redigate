// store.go is the Redis exact-match cache. Values are raw upstream response
// bodies. TTL is fixed for the process.
package cache

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// Store gets and sets cached completion bodies.
type Store struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewStore binds a Redis client and a positive TTL.
func NewStore(rdb *redis.Client, ttl time.Duration) *Store {
	return &Store{rdb: rdb, ttl: ttl}
}

// Get returns the cached body. ok is false on a miss. A Redis error is
// returned to the caller, which fails open.
func (s *Store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	b, err := s.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// Set stores a response body until the configured TTL.
func (s *Store) Set(ctx context.Context, key string, value []byte) error {
	return s.rdb.Set(ctx, key, value, s.ttl).Err()
}

// Ping checks that Redis is reachable. GET /readyz uses it.
func (s *Store) Ping(ctx context.Context) error {
	return s.rdb.Ping(ctx).Err()
}
