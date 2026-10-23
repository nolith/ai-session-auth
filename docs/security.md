# Security and isolation

geas manages credential lifecycles; **it is not a sandbox**. Isolation is
the job of a sandbox around the harness, and each session tells it what to
protect.

## What geas does

- The agent gets only short-lived, scoped credentials: a GitHub App user
  token, a fine-grained GitLab PAT per session, an SSH certificate of at
  most 12 hours.
- The GitLab issuer and the GitHub refresh token are never passed to the
  harness.
- Both providers verify the username before a credential is handed over.
- Tokens never appear in normal output, command-line arguments or Git URLs.
- Secret files are owner-only and written atomically; GitHub state updates
  are locked across sessions.
- Session PATs never get PAT management permissions, `ALL_MEMBERSHIPS` or
  `INSTANCE`.
- A 403 never causes automatic privilege expansion.

## What it does not do

Without a sandbox, an agent running as your OS user can read the mode-600
secret files, your own `glab` and `gh` logins, and every session's
directory. With the glab issuer, it can run `glab` with your own login by
unsetting `GLAB_CONFIG_DIR`. Isolation needs a sandbox that denies those
paths, or a separate OS user.

The session directory intentionally gives the harness its tokens. Any
process in the session can read them, use them with any tool, and copy
them: a copied GitLab PAT works until it is revoked at exit, a copied
GitHub token until it expires, within eight hours.

The credential helper's allowlist decides where Git gets a token; it does
not restrict API calls. The tokens themselves are the boundary:

- GitLab enforces the session PAT's granular scopes, which name only the
  configured namespaces;
- GitHub enforces the repositories the App is installed on and the App's
  permissions. `owners` and `repositories` do not narrow the token's API
  access, so install the App only on the repositories you need.

## The sandbox policy

Each session writes the paths a sandbox must protect into its directory:

- `sandbox/policy.json`, neutral:
  - `deny`: your glab and gh logins, a stored issuer PAT, the GitHub App
    state and its lock, every agent's SSH key in
    `$XDG_STATE_HOME/geas/agents`, and `$XDG_STATE_HOME/geas` as a whole;
  - `read` and `reopen`: the session's own directory, inside the denied
    state directory.
- `sandbox/nono.json`: the same, as a
  [nono](https://github.com/nolabs-ai/nono) profile fragment with
  `filesystem.deny`, `read` and `bypass_protection`, and no `extends`: the
  launcher that runs nono names its own base profile.
  `GEAS_SANDBOX_PROFILE` holds its path.

geas finds each tool's directory as the tool does:

- glab: `GLAB_CONFIG_DIR`, else `$XDG_CONFIG_HOME/glab-cli`, else the OS's
  user configuration directory (`~/Library/Application Support/glab-cli`
  on macOS);
- gh: `GH_CONFIG_DIR`, else `$XDG_CONFIG_HOME/gh`, else `~/.config/gh`.

## Checking the sandbox

`geas sandbox check`, run inside a session, tries to read every path the
policy denies and the session directory. It names each denied path it could
read, and exits 1 if any was readable or the session directory was not.
Run outside a sandbox, it fails by design: your own logins are readable
there.

With a `wrapper`, every session runs the check itself, in the background
once the harness has started, as `wrapper.command... geas sandbox check`,
with the session's environment, in a session of its own without a
terminal. Its verdict is the `sandbox` line of `geas status`: `pending`
until it ends, then `ready`, or `failed` with the paths it could read (or
why it could not run), which also makes `geas status` exit 1. Nothing it or
the wrapper prints reaches the terminal. Without a wrapper there is nothing
to check, and no line.

## Known limits

- The included GitLab permissions are an initial profile based on provider
  documentation. Some `glab` and `gh` commands make extra queries and may
  need more permissions.
- Git LFS, and harnesses with special terminal behaviour, are not tested:
  verify them locally.
- The test suite simulates the provider APIs, with no real credentials:
  run the [quick start](../README.md#quick-start) verification on your own
  account first, and test pushes, comments, pull and merge requests and
  pipeline control on a test repository.
