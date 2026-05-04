package sizes

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gitproto"
	"github.com/cloudflare/artifact-fs/internal/model"
)

type memCache struct {
	mu     sync.Mutex
	sizes  map[string]int64
	source map[string]string
	gets   atomic.Int64
	puts   atomic.Int64
}

func newMemCache() *memCache {
	return &memCache{sizes: map[string]int64{}, source: map[string]string{}}
}

func (m *memCache) GetOIDSize(_ context.Context, oid string) (int64, bool, error) {
	m.gets.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sizes[oid]
	return s, ok, nil
}

func (m *memCache) PutOIDSize(_ context.Context, oid string, size int64, source string, _ int64) error {
	m.puts.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sizes[oid] = size
	m.source[oid] = source
	return nil
}

type countingProto struct {
	mu      sync.Mutex
	calls   atomic.Int32
	answer  map[string]int64
	err     error
	gate    chan struct{}
	delayCh chan struct{}
}

func (p *countingProto) ObjectSizes(ctx context.Context, oids []string) (map[string]int64, error) {
	p.calls.Add(1)
	if p.gate != nil {
		<-p.gate
	}
	if p.err != nil {
		return nil, p.err
	}
	out := map[string]int64{}
	p.mu.Lock()
	for _, oid := range oids {
		if v, ok := p.answer[oid]; ok {
			out[oid] = v
		}
	}
	p.mu.Unlock()
	return out, nil
}

type stubFallback struct {
	calls atomic.Int32
	size  int64
	err   error
}

func (f *stubFallback) ResolveBlobSize(_ context.Context, _ model.RepoConfig, _ string) (int64, error) {
	f.calls.Add(1)
	return f.size, f.err
}

func protoFactory(p ProtoClient) ProtoFactory {
	return func(_ model.RepoConfig) ProtoClient { return p }
}

func TestResolveCacheHitShortCircuits(t *testing.T) {
	cache := newMemCache()
	cache.PutOIDSize(context.Background(), "abc", 42, "object-info", time.Now().Unix())

	proto := &countingProto{answer: map[string]int64{"abc": 999}}
	fb := &stubFallback{size: 0, err: errors.New("fallback should not run")}
	r := New(cache, protoFactory(proto), fb, nil)

	got, err := r.ResolveSize(context.Background(), model.RepoConfig{}, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Fatalf("got %d, want cached 42", got)
	}
	if proto.calls.Load() != 0 {
		t.Fatalf("proto called %d times on cache hit", proto.calls.Load())
	}
	if fb.calls.Load() != 0 {
		t.Fatalf("fallback called %d times on cache hit", fb.calls.Load())
	}
}

func TestResolveSingleflightCollapsesConcurrentCalls(t *testing.T) {
	cache := newMemCache()
	gate := make(chan struct{})
	proto := &countingProto{
		answer: map[string]int64{"abc": 17},
		gate:   gate,
	}
	r := New(cache, protoFactory(proto), &stubFallback{}, nil)

	const n = 16
	var wg sync.WaitGroup
	results := make([]int64, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = r.ResolveSize(context.Background(), model.RepoConfig{}, "abc")
		}()
	}
	// Give callers a moment to enter the singleflight group, then release the
	// proto call.
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("call %d: %v", i, e)
		}
		if results[i] != 17 {
			t.Fatalf("call %d: got %d, want 17", i, results[i])
		}
	}
	if got := proto.calls.Load(); got != 1 {
		t.Fatalf("proto called %d times, want 1 (singleflight)", got)
	}
}

func TestResolveFallsBackWhenProtocolUnsupported(t *testing.T) {
	cache := newMemCache()
	proto := &countingProto{err: gitproto.ErrUnsupported}
	fb := &stubFallback{size: 11}
	r := New(cache, protoFactory(proto), fb, nil)

	got, err := r.ResolveSize(context.Background(), model.RepoConfig{}, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if got != 11 {
		t.Fatalf("got %d, want 11", got)
	}
	if fb.calls.Load() != 1 {
		t.Fatalf("fallback calls = %d, want 1", fb.calls.Load())
	}
	// Result should be cached for next time.
	cached, ok, _ := cache.GetOIDSize(context.Background(), "abc")
	if !ok || cached != 11 {
		t.Fatalf("expected cache to record fallback result, got ok=%v size=%d", ok, cached)
	}
}

func TestResolveFallbackFailureDoesNotPoisonCache(t *testing.T) {
	cache := newMemCache()
	proto := &countingProto{err: gitproto.ErrUnsupported}
	fb := &stubFallback{err: errors.New("boom")}
	r := New(cache, protoFactory(proto), fb, nil)

	_, err := r.ResolveSize(context.Background(), model.RepoConfig{}, "abc")
	if err == nil {
		t.Fatal("expected error")
	}
	if cache.puts.Load() != 0 {
		t.Fatalf("cache puts on failure = %d, want 0", cache.puts.Load())
	}
	// A subsequent successful call should populate the cache normally.
	fb.err = nil
	fb.size = 5
	got, err := r.ResolveSize(context.Background(), model.RepoConfig{}, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if got != 5 {
		t.Fatalf("got %d, want 5", got)
	}
}

func TestResolveServerWithoutOIDFallsBack(t *testing.T) {
	cache := newMemCache()
	proto := &countingProto{answer: map[string]int64{}} // server doesn't have the oid
	fb := &stubFallback{size: 9}
	r := New(cache, protoFactory(proto), fb, nil)

	got, err := r.ResolveSize(context.Background(), model.RepoConfig{}, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if got != 9 {
		t.Fatalf("got %d, want 9", got)
	}
}
