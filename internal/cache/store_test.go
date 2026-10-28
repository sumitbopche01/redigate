package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestStoreMissSetHit(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	s := NewStore(rdb, time.Hour)
	ctx := context.Background()

	if err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	_, ok, err := s.Get(ctx, "redigate:chat:v1:missing")
	if err != nil || ok {
		t.Fatalf("miss ok=%v err=%v", ok, err)
	}
	if err := s.Set(ctx, "redigate:chat:v1:abc", []byte(`{"id":"1"}`)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get(ctx, "redigate:chat:v1:abc")
	if err != nil || !ok {
		t.Fatalf("hit ok=%v err=%v", ok, err)
	}
	if string(got) != `{"id":"1"}` {
		t.Fatalf("body %s", got)
	}
}

func TestStoreExpires(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	s := NewStore(rdb, time.Second)
	ctx := context.Background()
	if err := s.Set(ctx, "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(2 * time.Second)
	_, ok, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected key to expire")
	}
}

func TestStorePingDown(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	s := NewStore(rdb, time.Second)
	mr.Close()
	if err := s.Ping(context.Background()); err == nil {
		t.Fatal("expected ping failure")
	}
}
