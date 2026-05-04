package daemon

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/gitproto"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/sizes"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

// mockObjectInfoServer mimics enough of git's smart-HTTP protocol v2 to
// answer object-info=size requests. Tracks request counts per oid so the
// integration test can assert no full-blob fetches were issued.
type mockObjectInfoServer struct {
	sizes        map[string]int64
	infoRefsHits atomic.Int32
	uploadHits   atomic.Int32
}

func (m *mockObjectInfoServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.RequestURI, "/info/refs"):
			m.infoRefsHits.Add(1)
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			fmt.Fprintf(w, "%04x# service=git-upload-pack\n", len("# service=git-upload-pack\n")+4)
			w.Write([]byte("0000"))
			writePkt(w, "version 2\n")
			writePkt(w, "agent=mock/0.1\n")
			writePkt(w, "object-info=size\n")
			w.Write([]byte("0000"))
		case r.RequestURI == "/git-upload-pack":
			m.uploadHits.Add(1)
			body, _ := readAll(r.Body)
			oids := parseOIDs(body)
			var resp bytes.Buffer
			writePkt(&resp, "size\n")
			for _, oid := range oids {
				if size, ok := m.sizes[oid]; ok {
					writePkt(&resp, fmt.Sprintf("%s %d\n", oid, size))
				}
			}
			resp.WriteString("0000")
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			w.Write(resp.Bytes())
		default:
			http.NotFound(w, r)
		}
	}
}

func writePkt(w bytesWriter, s string) {
	fmt.Fprintf(w, "%04x", len(s)+4)
	w.Write([]byte(s))
}

type bytesWriter interface {
	Write([]byte) (int, error)
}

func readAll(r interface {
	Read([]byte) (int, error)
}) ([]byte, error) {
	var buf bytes.Buffer
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			break
		}
	}
	return buf.Bytes(), nil
}

func parseOIDs(body []byte) []string {
	var out []string
	i := 0
	for i+4 <= len(body) {
		var n int
		fmt.Sscanf(string(body[i:i+4]), "%04x", &n)
		i += 4
		if n == 0 || n == 1 {
			continue
		}
		if n < 4 || i+n-4 > len(body) {
			return out
		}
		line := string(body[i : i+n-4])
		i += n - 4
		if rest, ok := strings.CutPrefix(line, "oid "); ok {
			out = append(out, strings.TrimRight(rest, "\n"))
		}
	}
	return out
}

// TestResolverFillsUnknownSizesViaObjectInfo covers the end-to-end PR 2 flow:
// a snapshot has SizeState="unknown" rows; FUSE Getattr resolves the size via
// the protocol-v2 mock; result is persisted to the snapshot row; a second
// Getattr returns the same answer without re-hitting the network.
func TestResolverFillsUnknownSizesViaObjectInfo(t *testing.T) {
	const (
		oid1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		oid2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	mock := &mockObjectInfoServer{sizes: map[string]int64{oid1: 1234, oid2: 5678}}
	hs := httptest.NewServer(mock.handler())
	defer hs.Close()

	dir := t.TempDir()
	ctx := context.Background()
	snap, err := snapshot.New(ctx, filepath.Join(dir, "snap.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()

	gen, err := snap.PublishGeneration(ctx, "head1", "main", []model.BaseNode{
		{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: oid1, SizeState: "unknown"},
		{RepoID: "r", Path: "b.txt", Type: "file", Mode: 0o644, ObjectOID: oid2, SizeState: "unknown"},
	})
	if err != nil {
		t.Fatal(err)
	}

	repo := model.RepoConfig{ID: "r", Name: "r", RemoteURL: hs.URL, RemoteName: "origin"}
	ov, err := overlay.New(ctx, model.RepoConfig{
		ID: "r", Name: "r",
		OverlayDir:    filepath.Join(dir, "ov"),
		OverlayDBPath: filepath.Join(dir, "ov.sqlite"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ov.Close()

	gs := gitstore.New(nil)
	defer gs.Close()
	factory := func(r model.RepoConfig) sizes.ProtoClient {
		return gitproto.NewClient(r.RemoteURL)
	}
	// Use a dedicated fallback that fails loudly so we can prove the test
	// goes through the protocol-v2 path.
	failFallback := failingFallback{t: t}
	sr := sizes.New(snap, factory, failFallback, nil)

	resolver := &fusefs.Resolver{Snapshot: snap, Overlay: ov, Sizes: sr, Repo: repo}
	resolver.SetGeneration(gen)

	_, size, _, _, err := resolver.Getattr(ctx, "a.txt")
	if err != nil {
		t.Fatalf("Getattr a.txt: %v", err)
	}
	if size != 1234 {
		t.Fatalf("a.txt size = %d, want 1234", size)
	}
	_, size, _, _, err = resolver.Getattr(ctx, "b.txt")
	if err != nil {
		t.Fatal(err)
	}
	if size != 5678 {
		t.Fatalf("b.txt size = %d, want 5678", size)
	}

	// Snapshot row should now be denormalized to known.
	n, ok := snap.GetNode(gen, "a.txt")
	if !ok || n.SizeState != "known" || n.SizeBytes != 1234 {
		t.Fatalf("snapshot row not updated: %+v ok=%v", n, ok)
	}

	// Subsequent Getattr should hit the persistent oid_sizes cache; clear the
	// snapshot row size to force resolution again and confirm no extra
	// upload-pack hits.
	uploadBefore := mock.uploadHits.Load()
	// Re-publish with unknown to simulate a fresh generation reusing the same OID.
	gen2, err := snap.PublishGeneration(ctx, "head2", "main", []model.BaseNode{
		{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: oid1, SizeState: "unknown"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver.SetGeneration(gen2)
	_, size, _, _, err = resolver.Getattr(ctx, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if size != 1234 {
		t.Fatalf("a.txt re-resolved size = %d, want 1234", size)
	}
	if got := mock.uploadHits.Load(); got != uploadBefore {
		t.Fatalf("upload-pack hits jumped from %d to %d, want cache hit instead", uploadBefore, got)
	}
}

type failingFallback struct{ t *testing.T }

func (f failingFallback) ResolveBlobSize(_ context.Context, _ model.RepoConfig, oid string) (int64, error) {
	f.t.Fatalf("fallback should not be called for oid %s", oid)
	return 0, nil
}
