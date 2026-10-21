// Package directory defines how the engine reads an association's member directory, and
// the registry that lets new ways of reading one be added without changing the engine.
//
// # Three ways to add a source, in order of preference
//
// 1. Configuration only. The declarative HTML adapter is driven by CSS selectors and
// pagination rules written in the association's YAML file. Most public member directories
// are a repeating block of rows, so most new sources need no code at all — in any language.
// This is the route to reach for first.
//
// 2. A compile-time adapter. A separate Go module can implement [Adapter] and call
// [Register] from an init function; a small main that imports both packages gets the new
// adapter. That keeps a bespoke adapter — and its own tests, CI and release cycle — in its
// own repository, while the engine stays unchanged.
//
// 3. An external adapter. The exec adapter runs any binary or script and reads
// [model.DirectoryRecord] values as JSON from its stdout. An adapter can therefore be
// written in any language and shipped in any repository, which is what keeps a Go engine
// from being a closed shop.
//
// See docs/adapter-protocol.md for the contract each route has to honour.
package directory

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

// Page is a fetched document, as handed to an adapter. Adapters never make their own HTTP
// requests: they are given a [Fetcher], so the engine's safety checks, rate limits, caching
// and robots handling apply to every request without each adapter remembering to apply them.
type Page struct {
	// URL is the URL actually fetched, after redirects.
	URL string
	// RequestedURL is what the adapter asked for.
	RequestedURL string
	Status       int
	ContentType  string
	Body         []byte
	FetchedAt    time.Time
	// FromCache is true when the body came from the local cache rather than the network.
	FromCache bool
}

// Fetcher is the only way an adapter may reach the network.
type Fetcher interface {
	// Get returns the page at rawURL, or an error. Implementations enforce the URL safety
	// rules, the per-domain rate limit, the response size cap and robots directives.
	Get(ctx context.Context, rawURL string) (*Page, error)
}

// Request is what an adapter is asked to do.
type Request struct {
	// Association is the configured source, including its adapter-specific Settings.
	Association model.Association
	// Fetcher is nil for adapters whose access method is "manual".
	Fetcher Fetcher
	// Limits are the effective limits for this run, with engine defaults already applied.
	Limits model.CrawlLimits
	// Logger is for progress and diagnostics. Never log a full page body or a contact.
	Logger *slog.Logger
}

// Blocked records a source that refused automated access.
//
// It is a result, not an error, and it is never worked around. A directory behind a login,
// a CAPTCHA or a hard refusal is reported so a human can decide whether there is a
// legitimate route to the same data — an official export, an API, or asking the
// association. See docs/compliance.md.
type Blocked struct {
	URL    string
	Reason string
}

// Result is what an adapter produces.
type Result struct {
	Records []model.DirectoryRecord
	// PagesFetched counts requests actually made, for the run's statistics.
	PagesFetched int
	// Blocked lists sources that refused access.
	Blocked []Blocked
	// Warnings are problems that did not stop the run: a row missing a field, a page whose
	// shape did not match the configured selectors. Reported rather than swallowed,
	// because silent partial success is how a broken selector goes unnoticed for weeks.
	Warnings []string
	// Truncated is true when a configured limit stopped the run before the directory ended,
	// so a caller never mistakes a capped run for a complete one.
	Truncated bool
}

// Adapter reads one kind of directory.
//
// An adapter's job is narrow on purpose: produce [model.DirectoryRecord] values with their
// provenance filled in. It does not normalise names, resolve identities, deduplicate, score
// or decide what a company is — the engine does that once, for every adapter, so that one
// dedup policy applies everywhere.
type Adapter interface {
	// Name is the identifier an association's `adapter:` field names. It must match the
	// name the adapter is registered under.
	Name() string

	// Discover reads the directory described by the request.
	//
	// Partial success is expected and must be returned, not discarded: a Result with some
	// records, some warnings and a Blocked entry is a useful outcome. Return an error only
	// when nothing could be read at all.
	Discover(ctx context.Context, req Request) (Result, error)
}

// Factory builds a fresh adapter. Adapters are built per run so that one holding state
// cannot leak it into the next association.
type Factory func() Adapter

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register makes an adapter available under name.
//
// It is safe for concurrent use and intended to be called from an init function, which is
// what lets a separately released Go module add an adapter by being imported. Registering
// the same name twice panics: two adapters silently competing for one name would make
// `eucd discover` do something different depending on import order.
func Register(name string, factory Factory) {
	if name == "" {
		panic("directory.Register: empty adapter name")
	}
	if factory == nil {
		panic("directory.Register: nil factory for " + name)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		panic("directory.Register: adapter " + name + " is already registered")
	}
	registry[name] = factory
}

// Get builds the adapter registered under name.
func Get(name string) (Adapter, error) {
	registryMu.RLock()
	factory, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no adapter named %q is registered; available: %v", name, Names())
	}
	adapter := factory()
	if adapter.Name() != name {
		return nil, fmt.Errorf("adapter registered as %q reports its name as %q",
			name, adapter.Name())
	}
	return adapter, nil
}

// Names lists the registered adapter names, sorted. `eucd adapters` prints this, and
// `eucd config validate` checks association files against it.
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// unregisterForTest removes an adapter. Only tests use it, so that registering a fake
// adapter does not leak into other tests.
func unregisterForTest(name string) {
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(registry, name)
}

// Setting reads a typed value from an association's adapter-specific settings.
//
// Adapter settings arrive from YAML as map[string]any, so every adapter needs the same
// small amount of defensive reading. Doing it here means a misconfigured selector produces
// one clear message instead of a panic inside an adapter.
func Setting[T any](settings map[string]any, key string) (T, bool) {
	var zero T
	raw, ok := settings[key]
	if !ok {
		return zero, false
	}
	typed, ok := raw.(T)
	if !ok {
		return zero, false
	}
	return typed, true
}

// SettingString reads a string setting, or fallback when it is absent or not a string.
func SettingString(settings map[string]any, key, fallback string) string {
	if value, ok := Setting[string](settings, key); ok && value != "" {
		return value
	}
	return fallback
}

// SettingInt reads an integer setting. YAML numbers decode as int, so both int and float64
// are accepted.
func SettingInt(settings map[string]any, key string, fallback int) int {
	switch raw := settings[key].(type) {
	case int:
		return raw
	case int64:
		return int(raw)
	case float64:
		return int(raw)
	default:
		return fallback
	}
}
