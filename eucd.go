// Package eucd is the root of the eu-company-discovery engine.
//
// The engine discovers companies through industry association directories, resolves their
// official websites, extracts contacts those companies published themselves, and ranks them
// against a configurable profile. Countries, sectors and sources are configuration; the ways
// of reading a directory are adapters resolved at run time.
//
// Nothing here reads a CV, writes an application or sends mail. Those belong to a private
// consumer that reads this engine's versioned export — which is why this repository can be
// public and hold no personal data at all.
//
// The packages:
//
//   - [github.com/iobyte-labs/eu-company-discovery/pkg/model] — the shared data types.
//   - [github.com/iobyte-labs/eu-company-discovery/pkg/normalize] — identity and dedup keys.
//   - [github.com/iobyte-labs/eu-company-discovery/pkg/config] — country, sector and
//     association configuration.
//   - [github.com/iobyte-labs/eu-company-discovery/pkg/store] — SQLite persistence and
//     migrations.
//   - [github.com/iobyte-labs/eu-company-discovery/pkg/fetch] — HTTP that is safe to point
//     at an untrusted URL.
//   - [github.com/iobyte-labs/eu-company-discovery/pkg/directory] — the adapter contract
//     and registry.
//   - [github.com/iobyte-labs/eu-company-discovery/pkg/export] — the versioned data
//     contract.
package eucd

import "github.com/iobyte-labs/eu-company-discovery/pkg/model"

// Version is this build's version. Set at release time; the Makefile stamps it from the git
// tag for released binaries.
var Version = "0.1.0-dev"

// ExportSchemaVersion identifies the shape of exported JSON. A consumer checks this before
// trusting a file. See [github.com/iobyte-labs/eu-company-discovery/pkg/export].
const ExportSchemaVersion = model.ExportSchemaVersion
