package gitproto

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	oidA = "0000000000000000000000000000000000000000"
	oidB = "1111111111111111111111111111111111111111"
	oidC = "2222222222222222222222222222222222222222"
)

type cannedServer struct {
	infoRefs []byte
	upload   func(reqBody []byte) ([]byte, int)
	gotURI   []string
}

func (s *cannedServer) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		s.gotURI = append(s.gotURI, r.RequestURI)
		switch {
		case strings.HasPrefix(r.RequestURI, "/info/refs"):
			if r.Header.Get("Git-Protocol") != "version=2" {
				t.Errorf("missing Git-Protocol header: got %q", r.Header.Get("Git-Protocol"))
			}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			w.Write(s.infoRefs)
		case r.RequestURI == "/git-upload-pack":
			body, _ := io.ReadAll(r.Body)
			out, status := s.upload(body)
			if status == 0 {
				status = http.StatusOK
			}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			w.WriteHeader(status)
			w.Write(out)
		default:
			http.NotFound(w, r)
		}
	}
}

func encodePktLines(payloads ...string) []byte {
	var buf bytes.Buffer
	for _, p := range payloads {
		switch p {
		case "FLUSH":
			buf.WriteString("0000")
		case "DELIM":
			buf.WriteString("0001")
		default:
			fmt.Fprintf(&buf, "%04x%s", len(p)+4, p)
		}
	}
	return buf.Bytes()
}

func TestObjectSizesAdvertisedCapability(t *testing.T) {
	srv := &cannedServer{
		infoRefs: encodePktLines(
			"# service=git-upload-pack\n",
			"FLUSH",
			"version 2\n",
			"agent=git/2.40.0\n",
			"object-info=size\n",
			"FLUSH",
		),
		upload: func(_ []byte) ([]byte, int) {
			return encodePktLines(
				"size\n",
				oidA+" 100\n",
				oidB+" 250\n",
				"FLUSH",
			), http.StatusOK
		},
	}
	hs := httptest.NewServer(srv.handler(t))
	defer hs.Close()
	c := NewClient(hs.URL)

	got, err := c.ObjectSizes(context.Background(), []string{oidA, oidB})
	if err != nil {
		t.Fatalf("ObjectSizes: %v", err)
	}
	if got[oidA] != 100 || got[oidB] != 250 {
		t.Fatalf("sizes mismatch: %+v", got)
	}
}

func TestObjectSizesUnsupportedCapability(t *testing.T) {
	srv := &cannedServer{
		infoRefs: encodePktLines(
			"# service=git-upload-pack\n",
			"FLUSH",
			"version 2\n",
			"agent=git/2.20.0\n",
			"ls-refs=unborn\n",
			"fetch=shallow\n",
			"FLUSH",
		),
		upload: func(_ []byte) ([]byte, int) {
			t.Fatal("upload-pack should not be called when capability missing")
			return nil, 0
		},
	}
	hs := httptest.NewServer(srv.handler(t))
	defer hs.Close()
	c := NewClient(hs.URL)

	_, err := c.ObjectSizes(context.Background(), []string{oidA})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestObjectSizesPartialResponse(t *testing.T) {
	srv := &cannedServer{
		infoRefs: encodePktLines(
			"version 2\n",
			"object-info=size\n",
			"FLUSH",
		),
		upload: func(_ []byte) ([]byte, int) {
			// Server only knows oidA; oidC line has no size token.
			return encodePktLines(
				"size\n",
				oidA+" 7\n",
				oidC+"\n",
				"FLUSH",
			), http.StatusOK
		},
	}
	hs := httptest.NewServer(srv.handler(t))
	defer hs.Close()
	c := NewClient(hs.URL)

	got, err := c.ObjectSizes(context.Background(), []string{oidA, oidC})
	if err != nil {
		t.Fatalf("ObjectSizes: %v", err)
	}
	if len(got) != 1 || got[oidA] != 7 {
		t.Fatalf("expected only oidA in map, got %+v", got)
	}
}

func TestObjectSizesMalformedPktLine(t *testing.T) {
	srv := &cannedServer{
		infoRefs: encodePktLines(
			"version 2\n",
			"object-info=size\n",
			"FLUSH",
		),
		upload: func(_ []byte) ([]byte, int) {
			// Length prefix "ZZZZ" is not valid hex.
			return []byte("ZZZZsize\n0000"), http.StatusOK
		},
	}
	hs := httptest.NewServer(srv.handler(t))
	defer hs.Close()
	c := NewClient(hs.URL)

	_, err := c.ObjectSizes(context.Background(), []string{oidA})
	if err == nil {
		t.Fatal("expected error on malformed pkt-line")
	}
}

func TestObjectSizesNonHTTPSchemeIsUnsupported(t *testing.T) {
	c := NewClient("git@github.com:org/repo.git")
	_, err := c.ObjectSizes(context.Background(), []string{oidA})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for ssh-style url, got %v", err)
	}
}

func TestObjectSizesEmptyOIDList(t *testing.T) {
	c := NewClient("https://example.com/repo.git")
	got, err := c.ObjectSizes(context.Background(), nil)
	if err != nil {
		t.Fatalf("ObjectSizes: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %+v", got)
	}
}

func TestObjectSizesRequestEncodesOIDs(t *testing.T) {
	var seenBody []byte
	srv := &cannedServer{
		infoRefs: encodePktLines(
			"version 2\n",
			"object-info=size\n",
			"FLUSH",
		),
		upload: func(body []byte) ([]byte, int) {
			seenBody = body
			return encodePktLines("size\n", oidA+" 1\n", "FLUSH"), http.StatusOK
		},
	}
	hs := httptest.NewServer(srv.handler(t))
	defer hs.Close()
	c := NewClient(hs.URL)

	if _, err := c.ObjectSizes(context.Background(), []string{oidA}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(seenBody, []byte("command=object-info")) {
		t.Fatalf("request missing command line: %q", seenBody)
	}
	if !bytes.Contains(seenBody, []byte("oid "+oidA)) {
		t.Fatalf("request missing oid line: %q", seenBody)
	}
}
