# Bringing your own mail credentials, and sending from CI

This document covers one decision: the job seeker enters their Gmail credentials in a web
UI, the credentials are kept in their browser, and clicking **Send** makes CI send the
applications.

That is implementable and secure, but **the obvious way to build it leaks the password**.
This document says why, and what to do instead.

## The trap: never pass a credential as a workflow_dispatch input

The natural design is to collect the app password in the browser and hand it to the
workflow when triggering it:

```jsonc
// POST /repos/OWNER/REPO/actions/workflows/send.yml/dispatches
{
  "ref": "main",
  "inputs": { "smtp_password": "abcd efgh ijkl mnop" }   // ← DO NOT DO THIS
}
```

`workflow_dispatch` inputs are **not secrets**. They are:

- recorded in the workflow run's metadata and readable through the REST API by anyone with
  read access to the repository,
- shown in the Actions UI on the run's summary page,
- expanded into `github.event.inputs` and routinely echoed into logs by `set -x`, `env`
  dumps, or any step that prints its inputs,
- retained for as long as the run is retained.

In a **public** repository this publishes the password to the internet. In a **private**
one it hands it to every collaborator and to any token with `actions: read`. GitHub's own
secret redaction does not apply, because an input was never registered as a secret.

The same applies to passing a credential through `repository_dispatch`
`client_payload`, a branch name, a commit message, an issue body, or a file committed to
the repository.

## What to do instead

### Recommended: the browser writes an Actions secret, then dispatches with no payload

GitHub's REST API lets a client create an encrypted Actions secret. The browser encrypts
the value with the repository's own public key before it is sent, so the credential is
never in transit or at rest in readable form, and once stored it is a real secret —
redacted in logs and unreadable through the API.

The flow is:

1. `GET /repos/{owner}/{repo}/actions/secrets/public-key` → `{ key_id, key }`.
2. In the browser, seal the password with that key using a libsodium sealed box
   (`libsodium-wrappers`, or `tweetnacl`'s `box.seal`). This is the step that makes the
   whole design safe; do not substitute base64 or any hand-rolled encryption.
3. `PUT /repos/{owner}/{repo}/actions/secrets/SMTP_PASSWORD` with
   `{ encrypted_value, key_id }`.
4. `POST /repos/{owner}/{repo}/actions/workflows/send.yml/dispatches` with
   `{ "ref": "main", "inputs": { "max": "5", "dry_run": "false" } }` — **no credential in
   the payload**. The workflow reads `${{ secrets.SMTP_PASSWORD }}` as normal.

```js
// Step 2, in full. sodium is libsodium-wrappers, already awaited on sodium.ready.
const sealed = sodium.crypto_box_seal(
  sodium.from_string(appPassword),
  sodium.from_base64(publicKey, sodium.base64_variants.ORIGINAL),
);
const encrypted_value = sodium.to_base64(sealed, sodium.base64_variants.ORIGINAL);
```

The secret is written once, when the user saves their credentials, and rewritten only when
they change them. Clicking **Send** after that carries nothing sensitive.

### The token the browser needs, and its real cost

To write a secret and start a workflow, the browser needs a GitHub token with, on that one
repository:

- `secrets: write` — to store the credential,
- `actions: write` — to dispatch the workflow,
- `contents: read` — to read the queue.

Use a **fine-grained personal access token** scoped to that single repository, with an
expiry. Do not use a classic `repo` token: it grants everything, everywhere.

Be clear-eyed about what this means. A token that can write secrets and start workflows on
a repository is roughly as powerful as the password it protects — anyone holding it can
replace the secret and run a workflow that exfiltrates it. Keeping it in `localStorage`
means any cross-site scripting on the UI's origin can read it, and so can anyone with the
user's unlocked browser profile. That is an acceptable trade for a single-user tool the
user runs for themselves; it is not acceptable for a hosted service with several users. If
this ever becomes multi-user, the token has to move behind a server that holds it, or
become a GitHub App with a per-user installation.

### `localStorage` is a convenience, not storage

Keeping the app password in `localStorage` so the user does not retype it is reasonable,
with three conditions:

- Treat it as cached, never authoritative. The authoritative copy is the Actions secret.
- Offer an explicit **Forget credentials** button that clears it, and clear it on sign-out.
- Never log it, never put it in a URL, never send it anywhere except the sealed-box call
  above. A credential in a query string ends up in browser history, in proxy logs and in
  `Referer` headers.

### Alternative: send from the browser with the Gmail API, and skip CI

If the point of the UI is that the user is present and clicking, CI is not actually needed
to send. A browser cannot speak SMTP — it has no raw sockets — but it can call the Gmail
REST API directly with an OAuth access token:

- the user signs in with Google (OAuth, `gmail.send` scope only),
- the browser builds the MIME message and `POST`s it to
  `https://gmail.googleapis.com/gmail/v1/users/me/messages/send`,
- no app password exists, nothing is stored in a repository, and no GitHub token is needed.

This is the strongest option available for credentials: there is no long-lived password
anywhere, the scope is limited to sending, and the user can revoke access from their Google
account page. The predecessor project already proved this path — it has a working Gmail
OAuth transport alongside its SMTP one.

Its cost is that the browser tab must stay open for the duration of a batch, which matters
because applications are deliberately spaced minutes apart. For a run of five that is
fine; for thirty it is not.

### Honest comparison

| | Credential exposure | Needs a GitHub token | Unattended batches |
|---|---|---|---|
| Dispatch input | **Leaks — never use** | yes | yes |
| Browser → Actions secret → CI | App password at rest as a secret; powerful token in the browser | yes, fine-grained | yes |
| Browser → Gmail API | No password; revocable OAuth scope | no | no, tab must stay open |
| Local CLI, env vars | Password on the user's own machine only | no | yes, on their machine |

A reasonable combination: **browser → Gmail API** for an attended click-to-send of a few
applications, and the **local CLI** for a long unattended run. That needs no GitHub token
and no secret storage at all. Reach for the Actions-secret design when unattended sending
from CI is genuinely required.

## Rules that hold whichever option is chosen

1. **A credential is never an input, a payload, a filename, a branch, or a commit.** Only
   an encrypted secret, an OAuth token held in memory, or an environment variable on the
   user's own machine.
2. **No credential is ever written to a log.** Mail code prints which variables are *set*,
   never their values — the predecessor's `credential_report` is the pattern to copy.
3. **Sending stays behind an explicit human approval**, whatever triggers it. A UI click
   approves a reviewed batch; it does not approve "send whatever is queued from now on".
4. **The suppression list is checked at send time**, not only when the batch was built. A
   batch approved an hour ago may contain someone who has since opted out.
5. **Dry run is the default.** A workflow or command that sends must require an explicit
   flag to do so, so that a mis-click, a stray dispatch or a scheduled run that fires
   unexpectedly sends nothing.
6. **Rotate on exposure.** If a password ever reaches a log or an input, it is burned:
   revoke the Gmail app password immediately and issue a new one. Rotating is cheap;
   assuming it was not noticed is not.

## Checklist before wiring a UI to sending

- [ ] No credential appears in any `workflow_dispatch` or `repository_dispatch` payload.
- [ ] The secret is sealed with libsodium in the browser before leaving it.
- [ ] The GitHub token is fine-grained, single-repository, and has an expiry.
- [ ] The UI has a working "forget credentials" action.
- [ ] The send workflow defaults to dry run and needs an explicit flag to send.
- [ ] The send path re-checks the suppression list immediately before each message.
- [ ] A test run proves no credential appears in the workflow log.
