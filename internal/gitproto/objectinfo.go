// Package gitproto implements a minimal git protocol v2 client for the
// `object-info` command. It speaks smart-HTTP over HTTPS and uses pkt-line
// framing. The goal is to fetch blob sizes from a remote without transferring
// blob content.
package gitproto

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
)

// ErrUnsupported is returned when the remote does not advertise the
// `object-info` command, or when the remote URL uses a scheme this client
// cannot speak (anything other than http/https).
var ErrUnsupported = errors.New("server does not advertise object-info")

// AuthSource provides credentials for HTTPS basic auth, typically by shelling
// out to `git credential fill`. A nil AuthSource yields anonymous requests.
type AuthSource interface {
	Credentials(ctx context.Context, remoteURL string) (username, password string, err error)
}

// GitCredentialAuth wraps the system `git credential fill` helper. Use this
// for production resolution against private remotes.
type GitCredentialAuth struct{}

func (GitCredentialAuth) Credentials(ctx context.Context, remoteURL string) (string, string, error) {
	u, err := url.Parse(remoteURL)
	if err != nil {
		return "", "", err
	}
	if u.User != nil {
		if pw, ok := u.User.Password(); ok {
			return u.User.Username(), pw, nil
		}
	}
	cmd := exec.CommandContext(ctx, "git", "credential", "fill")
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	in := fmt.Sprintf("protocol=%s\nhost=%s\npath=%s\n\n", u.Scheme, u.Host, strings.TrimPrefix(u.Path, "/"))
	cmd.Stdin = strings.NewReader(in)
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("git credential fill: %w", err)
	}
	var user, pass string
	for line := range strings.SplitSeq(strings.TrimRight(string(out), "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "username":
			user = v
		case "password":
			pass = v
		}
	}
	return user, pass, nil
}

type Client struct {
	httpClient *http.Client
	remoteURL  string
	auth       AuthSource
	logger     *slog.Logger
}

type Option func(*Client)

func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }
func WithAuth(a AuthSource) Option         { return func(c *Client) { c.auth = a } }
func WithLogger(l *slog.Logger) Option     { return func(c *Client) { c.logger = l } }

// NewClient builds a protocol-v2 client targeting the given remote URL. Only
// http and https schemes are supported; other transports must be handled by
// the caller's fallback path.
func NewClient(remoteURL string, opts ...Option) *Client {
	c := &Client{
		httpClient: http.DefaultClient,
		remoteURL:  remoteURL,
		logger:     slog.Default(),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ObjectSizes returns the size in bytes of each OID the server can answer for.
// OIDs the server does not have are omitted from the returned map. Returns
// ErrUnsupported if the server does not advertise the `object-info` command,
// or if the remote uses an unsupported transport.
func (c *Client) ObjectSizes(ctx context.Context, oids []string) (map[string]int64, error) {
	if len(oids) == 0 {
		return map[string]int64{}, nil
	}
	if !isHTTP(c.remoteURL) {
		return nil, ErrUnsupported
	}
	if err := c.checkCapability(ctx); err != nil {
		return nil, err
	}
	return c.requestObjectInfo(ctx, oids)
}

func isHTTP(remoteURL string) bool {
	u, err := url.Parse(remoteURL)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func (c *Client) newRequest(ctx context.Context, method, suffix string, body io.Reader) (*http.Request, error) {
	u := strings.TrimRight(c.remoteURL, "/") + suffix
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Git-Protocol", "version=2")
	if c.auth != nil {
		user, pass, err := c.auth.Credentials(ctx, c.remoteURL)
		if err == nil && (user != "" || pass != "") {
			cred := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
			req.Header.Set("Authorization", "Basic "+cred)
		}
	}
	return req, nil
}

func (c *Client) checkCapability(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodGet, "/info/refs?service=git-upload-pack", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUnsupported
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("info/refs status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if !advertisesObjectInfoSize(body) {
		return ErrUnsupported
	}
	return nil
}

// advertisesObjectInfoSize parses the protocol-v2 capability advertisement and
// returns true if the server lists `object-info` with a `size` argument.
//
// Smart-HTTP wraps the advertisement with a leading "# service=git-upload-pack"
// pkt-line and a flush, then the v2 advertisement begins with "version 2".
// Bare-protocol responses (e.g., from a stub server) skip the service header.
// We tolerate both: scan all pkt-lines, ignore flush-pkts as section dividers.
func advertisesObjectInfoSize(body []byte) bool {
	r := bytes.NewReader(body)
	for {
		line, err := readPktLine(r)
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				return false
			}
			return false
		}
		if line == nil { // flush-pkt: section boundary, keep scanning
			if r.Len() == 0 {
				return false
			}
			continue
		}
		s := strings.TrimRight(string(line), "\n")
		if !strings.HasPrefix(s, "object-info") {
			continue
		}
		// Forms seen in practice:
		//   "object-info=size"
		//   "object-info"  (followed by a separate "size" line — rare)
		_, args, _ := strings.Cut(s, "=")
		if args == "" {
			return true // bare advertisement; let request fail loudly later
		}
		for arg := range strings.SplitSeq(args, " ") {
			if arg == "size" {
				return true
			}
		}
	}
}

func (c *Client) requestObjectInfo(ctx context.Context, oids []string) (map[string]int64, error) {
	body, err := buildObjectInfoRequest(oids)
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/git-upload-pack", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Accept", "application/x-git-upload-pack-result")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("git-upload-pack status %d", resp.StatusCode)
	}
	return parseObjectInfoResponse(resp.Body)
}

func buildObjectInfoRequest(oids []string) ([]byte, error) {
	var buf bytes.Buffer
	if err := writePktLine(&buf, []byte("command=object-info\n")); err != nil {
		return nil, err
	}
	if err := writePktLine(&buf, []byte("size\n")); err != nil {
		return nil, err
	}
	if err := writeDelimPkt(&buf); err != nil {
		return nil, err
	}
	for _, oid := range oids {
		if !isHexOID(oid) {
			return nil, fmt.Errorf("invalid oid %q", oid)
		}
		if err := writePktLine(&buf, fmt.Appendf(nil, "oid %s\n", oid)); err != nil {
			return nil, err
		}
	}
	if err := writeFlushPkt(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func parseObjectInfoResponse(r io.Reader) (map[string]int64, error) {
	br := bufio.NewReader(r)
	out := map[string]int64{}
	// The response opens with a header line "size\n" identifying the requested
	// info field. Skip lines until we hit oid entries; tolerate missing header.
	for {
		line, err := readPktLine(br)
		if err != nil {
			return nil, err
		}
		if line == nil {
			break // flush-pkt terminates
		}
		s := strings.TrimRight(string(line), "\n")
		if s == "size" {
			continue
		}
		fields := strings.Fields(s)
		if len(fields) < 1 {
			continue
		}
		if !isHexOID(fields[0]) {
			continue
		}
		if len(fields) < 2 {
			continue // server says "we don't have it"
		}
		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse size %q: %w", fields[1], err)
		}
		out[fields[0]] = size
	}
	return out, nil
}

// pkt-line framing: 4-hex length prefix followed by payload. Length includes
// the 4 prefix bytes. 0000 is flush-pkt; 0001 is delim-pkt (v2).

func writePktLine(w io.Writer, payload []byte) error {
	if len(payload) > 0xFFFF-4 {
		return fmt.Errorf("pkt-line payload too large: %d", len(payload))
	}
	if _, err := fmt.Fprintf(w, "%04x", len(payload)+4); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func writeFlushPkt(w io.Writer) error {
	_, err := w.Write([]byte("0000"))
	return err
}

func writeDelimPkt(w io.Writer) error {
	_, err := w.Write([]byte("0001"))
	return err
}

// readPktLine returns the payload of the next pkt-line. A nil payload signals
// flush-pkt (0000). Delim-pkt (0001) is skipped.
func readPktLine(r io.Reader) ([]byte, error) {
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		n, err := strconv.ParseUint(string(hdr[:]), 16, 32)
		if err != nil {
			return nil, fmt.Errorf("malformed pkt-line length %q: %w", hdr[:], err)
		}
		switch n {
		case 0:
			return nil, nil
		case 1:
			continue // delim, read next
		}
		if n < 4 {
			return nil, fmt.Errorf("malformed pkt-line length %d", n)
		}
		buf := make([]byte, n-4)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
}

func isHexOID(s string) bool {
	if len(s) < 4 || len(s) > 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
