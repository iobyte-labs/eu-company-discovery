package fetch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/directory"
	"github.com/iobyte-labs/eu-company-discovery/pkg/normalize"
)

// DefaultCacheTTL is how long a cached page is reused. A member directory and a company's
// contact page change on the order of months, so a week keeps repeated runs off other
// people's servers without serving anything stale enough to matter.
const DefaultCacheTTL = 7 * 24 * time.Hour

// cache is an on-disk store of fetched pages, keyed by the canonical URL.
//
// It exists for three reasons, in this order: a re-run after a crash must not refetch
// everything from somebody else's server; a test must be able to run offline; and an
// operator debugging an extraction must be able to look at the exact bytes a decision was
// made from.
type cache struct {
	dir string
	ttl time.Duration
	now func() time.Time
}

// cacheEntry is the stored form. Body is a []byte, so JSON carries it base64-encoded —
// which keeps the file one self-describing document rather than a sidecar pair that can
// come apart.
type cacheEntry struct {
	URL          string    `json:"url"`
	RequestedURL string    `json:"requested_url"`
	Status       int       `json:"status"`
	ContentType  string    `json:"content_type"`
	FetchedAt    time.Time `json:"fetched_at"`
	Body         []byte    `json:"body"`
}

func newCache(dir string, ttl time.Duration, now func() time.Time) (*cache, error) {
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create the page cache %q: %w", dir, err)
	}
	if ttl == 0 {
		ttl = DefaultCacheTTL
	}
	return &cache{dir: dir, ttl: ttl, now: now}, nil
}

// path shards on the first byte of the hash: a crawl can cache tens of thousands of pages,
// and one directory holding all of them makes every other tool that touches it slow.
func (c *cache) path(canonicalURL string) string {
	hash := normalize.StableHash(canonicalURL)
	return filepath.Join(c.dir, hash[:2], hash+".json")
}

// get returns the cached page for canonicalURL, if one is present and fresh.
//
// Every failure is a miss, never an error: a corrupt or half-written cache file must cost
// one refetch, not a failed run.
func (c *cache) get(canonicalURL string) (*directory.Page, bool) {
	if c == nil {
		return nil, false
	}
	raw, err := os.ReadFile(c.path(canonicalURL))
	if err != nil {
		return nil, false
	}
	var entry cacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, false
	}
	if c.ttl > 0 && c.now().Sub(entry.FetchedAt) > c.ttl {
		return nil, false
	}
	return &directory.Page{
		URL:          entry.URL,
		RequestedURL: entry.RequestedURL,
		Status:       entry.Status,
		ContentType:  entry.ContentType,
		Body:         entry.Body,
		FetchedAt:    entry.FetchedAt,
		FromCache:    true,
	}, true
}

// put stores a page. A write failure is reported to the caller's logger and otherwise
// ignored: failing a crawl because a cache directory filled up would be the wrong trade.
func (c *cache) put(canonicalURL string, page *directory.Page) error {
	if c == nil || page == nil {
		return nil
	}
	encoded, err := json.Marshal(cacheEntry{
		URL:          page.URL,
		RequestedURL: page.RequestedURL,
		Status:       page.Status,
		ContentType:  page.ContentType,
		FetchedAt:    page.FetchedAt,
		Body:         page.Body,
	})
	if err != nil {
		return err
	}
	target := c.path(canonicalURL)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	// Write to a temporary file and rename, so a run interrupted mid-write leaves either
	// the old entry or the new one, never half a document that the next run would have to
	// recognise as junk.
	temp, err := os.CreateTemp(filepath.Dir(target), ".tmp-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	if _, err := temp.Write(encoded); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return err
	}
	if err := os.Rename(tempName, target); err != nil {
		_ = os.Remove(tempName)
		return err
	}
	return nil
}
