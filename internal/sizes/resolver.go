// Package sizes resolves blob sizes from a layered set of sources. It is the
// glue between FUSE Getattr and the slow paths that actually answer "how big
// is this blob": a persistent oid->size cache, a protocol-v2 object-info
// client, and a local-fetch fallback. Concurrent stats of the same OID are
// collapsed via singleflight.
package sizes

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gitproto"
	"github.com/cloudflare/artifact-fs/internal/model"
	"golang.org/x/sync/singleflight"
)

// SizeCache is the persistence boundary the resolver uses for OID sizes.
type SizeCache interface {
	GetOIDSize(ctx context.Context, oid string) (int64, bool, error)
	PutOIDSize(ctx context.Context, oid string, size int64, source string, resolvedAt int64) error
}

// ProtoClient is the protocol-v2 object-info client. The interface keeps the
// resolver decoupled from gitproto for testing.
type ProtoClient interface {
	ObjectSizes(ctx context.Context, oids []string) (map[string]int64, error)
}

// FallbackFetcher resolves a blob's size locally when protocol-v2 is
// unsupported or unreachable. Implementations typically shell out to git
// cat-file --batch-check and may fetch the blob's content.
type FallbackFetcher interface {
	ResolveBlobSize(ctx context.Context, repo model.RepoConfig, oid string) (int64, error)
}

// ProtoFactory builds a ProtoClient for a repo on demand. Returning a nil
// client means "no protocol-v2 attempt for this repo"; the resolver will go
// straight to the fallback.
type ProtoFactory func(repo model.RepoConfig) ProtoClient

type Resolver struct {
	cache    SizeCache
	proto    ProtoFactory
	fallback FallbackFetcher
	sf       singleflight.Group
	logger   *slog.Logger
}

func New(cache SizeCache, proto ProtoFactory, fallback FallbackFetcher, logger *slog.Logger) *Resolver {
	if logger == nil {
		logger = slog.Default()
	}
	return &Resolver{cache: cache, proto: proto, fallback: fallback, logger: logger}
}

// ResolveSize returns the size in bytes for the given OID. Resolution order:
// cache hit -> protocol v2 object-info -> local fallback. The first source to
// answer wins; the answer is then persisted to the cache. Concurrent calls
// for the same OID share one upstream lookup.
func (r *Resolver) ResolveSize(ctx context.Context, repo model.RepoConfig, oid string) (int64, error) {
	if oid == "" {
		return 0, errors.New("sizes: empty oid")
	}
	if size, ok, err := r.cache.GetOIDSize(ctx, oid); err == nil && ok {
		return size, nil
	} else if err != nil {
		r.logger.Debug("oid size cache lookup failed", "oid", oid, "error", err)
	}

	v, err, _ := r.sf.Do(oid, func() (any, error) {
		// Re-check the cache under singleflight so a slow first caller
		// can hand off to the cache write done by an even-earlier caller.
		if size, ok, err := r.cache.GetOIDSize(ctx, oid); err == nil && ok {
			return size, nil
		}
		size, source, err := r.resolveOnce(ctx, repo, oid)
		if err != nil {
			return int64(0), err
		}
		if putErr := r.cache.PutOIDSize(ctx, oid, size, source, time.Now().Unix()); putErr != nil {
			r.logger.Warn("oid size cache write failed", "oid", oid, "error", putErr)
		}
		return size, nil
	})
	if err != nil {
		return 0, err
	}
	return v.(int64), nil
}

// objectInfoDisabled is true when ARTIFACT_FS_DISABLE_OBJECT_INFO=1, forcing
// the resolver to skip the protocol-v2 path. Useful for testing the fallback.
func objectInfoDisabled() bool {
	return os.Getenv("ARTIFACT_FS_DISABLE_OBJECT_INFO") == "1"
}

func (r *Resolver) resolveOnce(ctx context.Context, repo model.RepoConfig, oid string) (int64, string, error) {
	if !objectInfoDisabled() && r.proto != nil {
		if pc := r.proto(repo); pc != nil {
			sizes, err := pc.ObjectSizes(ctx, []string{oid})
			if err == nil {
				if size, ok := sizes[oid]; ok {
					return size, "object-info", nil
				}
				// Server didn't have it; treat as unsupported and fall back.
			} else if !errors.Is(err, gitproto.ErrUnsupported) {
				r.logger.Debug("object-info request failed, falling back", "oid", oid, "error", err)
			}
		}
	}
	if r.fallback == nil {
		return 0, "", errors.New("sizes: no fallback available")
	}
	size, err := r.fallback.ResolveBlobSize(ctx, repo, oid)
	if err != nil {
		return 0, "", err
	}
	return size, "fetch", nil
}
