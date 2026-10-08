# Temporary credentials for AI agents, under your own identity

A Go launcher for GitHub.com and GitLab.com on Linux and macOS (amd64 and arm64).
It has no external Go dependencies. Build with Go 1.23 or later; use the latest
stable release where possible. Install `git`, `gh`, and `glab` in your `PATH`.

```bash
git clone https://github.com/nolith/ai-session-auth.git
cd ai-session-auth
go build -o agent-auth .
```

The compiled binary does not require a Go runtime.

Start an AI harness with API and Git HTTPS access under your own identity:

```bash
./agent-auth --config config.json run -- codex
```

Replace `codex` with any command and its arguments.
Standard GitHub and GitLab SSH remotes work without unlocking your SSH keyring.

## Features

- GitHub App **user access tokens**, acting as you rather than an installation bot.
- Initial authorization through device flow, without a client secret or RSA key.
- GitHub token refresh on demand, starting five minutes before expiry.
- A new **personal fine-grained GitLab PAT** for each session, issued through
  your own `glab` login or a stored issuer PAT.
- GitLab access restricted to selected projects, without PAT management permissions in the agent token.
- Username verification on both providers before starting the harness.
- `gh` and `glab` wrappers that retrieve the current token and invoke the original CLIs.
- A Git credential helper that checks protocol, hostname, and repository path.
- Temporary SSH-to-HTTPS URL rewriting through process-level Git configuration.
- GitLab PAT revocation on normal exit, Ctrl-C/SIGTERM, or the session time limit.
- Atomic GitHub state updates, private secret files, and locking across sessions.
- No tokens in normal launcher output, command-line arguments, or Git URLs.

## One-time GitHub setup

1. Open https://github.com/settings/apps and register a GitHub App.
2. Disable webhooks; this client does not use them. No public service is required.
3. Enable **Device flow** and keep user access token expiration enabled.
4. Configure these repository permissions:

| Permission | Level |
|---|---|
| Contents | Read and write |
| Issues | Read and write |
| Pull requests | Read and write |
| Actions | Read and write |
| Checks | Read-only |
| Commit statuses | Read-only |
| Metadata | Read-only, automatic |

To edit `.github/workflows/` files, also enable **Workflows: write**.
Leave Administration, Secrets, and protection bypass privileges disabled.

5. Install the App on your account or organization, selecting the required repositories.
   Organizations may require administrator approval and an active SSO session.
6. Copy the **Client ID**, not the App ID, into the configuration.
7. Set `expected_user` to your GitHub username.

User tokens can perform only operations allowed to both your user and the App.
Commit authorship also depends on `git config user.name/user.email`: the token
identifies the user performing the push; it does not rewrite the commit author.

## One-time GitLab.com setup

The **issuer** is the credential that creates, finds and revokes each
session's PAT: either your own `glab` login or a PAT you store.

### Issuer: your glab login

If `glab auth login` works for gitlab.com, set `"issuer": "glab"` in the
`gitlab` section and leave out `issuer_token_file`. The two cannot be
combined, and `save-gitlab-issuer` has nothing to save.

Every issuer request then runs `glab api --hostname gitlab.com ...`:

- Nothing is stored and nothing needs rotating. An OAuth login renews its own
  token, which lasts two hours, so revocation at exit still works in longer
  sessions. A copy of glab's current token would not.
- The login needs the `api` scope, which `glab auth login` requests. GitLab
  checks a new PAT against its creator's permissions only when the creator is
  itself fine-grained, so a legacy-scoped OAuth login can create it.
- glab is resolved on your `PATH` at start, runs from `/` and is pinned to
  gitlab.com: neither the current repository's remote nor glab's `:fullpath`
  style placeholders can redirect a request.
- glab runs without `GITLAB_TOKEN`, `GITLAB_ACCESS_TOKEN`, `OAUTH_TOKEN`,
  `CI_JOB_TOKEN`, `GLAB_CONFIG_DIR`, `AI_AUTH_SOCKET`, host overrides or HTTP
  debugging, so it acts with your own login, never with a session's.
- Calls are serialized through `$XDG_STATE_HOME/ai-session-auth/glab.lock`
  (`~/.local/state/ai-session-auth/glab.lock` by default). GitLab returns a
  new refresh token on every refresh, so two glab processes refreshing at once
  can race. The lock covers agent-auth's own calls, not yours.
- Each call gets one minute, so a keyring waiting to be unlocked fails the
  request instead of stalling the session.
- glab's output is never printed. A failure reports only "glab issuer request
  failed"; run the same `glab api` command yourself to see why.

If the login breaks (`glab auth logout`, a failed refresh, a revoked
authorization), sessions fail to start, and running sessions cannot revoke
their PATs, which then expire on their own. `glab auth login` fixes both.

### Issuer: a stored PAT

Open https://gitlab.com/-/user_settings/personal_access_tokens.
Create an **issuer PAT** for your own user, separate from the agent's session PATs.

Prefer a fine-grained issuer PAT with:

- In the **User** boundary: Personal Access Token **Create, Read, Revoke**;
  User **Read** to verify your identity.
- In the selected projects: at least every permission granted to the agent token,
  as listed in `gitlab.granular_scopes` in `config.example.json`.

The API permission names for the issuer's User boundary are:

```json
["create_personal_access_token", "read_personal_access_token", "revoke_personal_access_token", "read_user"]
```

A fine-grained issuer can create PATs only with permissions and resource
boundaries equal to or narrower than its own. Create PAT alone cannot issue a
push-capable token. Read and Revoke are needed for session cleanup.

Alternatively, bootstrap with your own legacy PAT with the `api` scope.
The token issued to the agent is **fine-grained** in either case.
The issuer remains subject to its own expiry and GitLab.com policies;
this version does not automatically renew the issuer credential.

### Projects

Find each project's numeric ID and update all three settings:

- `gitlab.repositories`: exact paths, such as `nolith/project`;
- `gitlab.project_ids`: numeric project IDs;
- `resourceIds`: `gid://gitlab/Project/ID` in the granular scopes.

The launcher queries the projects and verifies that their IDs and paths match.
The included profile covers code, issues and comments, MR creation and updates,
pipeline and job control, and artifact reads. Destructive, administrative, and
merge permissions are excluded from the initial profile. Git pushes remain
subject to your user permissions and the project's branch protections.

## Configuration and launch

```bash
cp config.example.json config.json
# Set the Client ID, usernames, repositories, and GitLab project IDs before proceeding.
./agent-auth --config config.json save-gitlab-issuer   # not with issuer "glab"
./agent-auth --config config.json github-login
./agent-auth --config config.json run -- codex
```

`save-gitlab-issuer` prompts for the PAT with hidden input. Do not put it in the
command line or configuration file. It saves the PAT at `issuer_token_file`
with mode 600. `github-login` displays a URL and a one-time code to authorize
in your browser; it does not print the returned tokens. It saves the OAuth
state at `state_file`.

Use absolute paths to launch the harness from another checkout:

```bash
/path/ai-session-auth/agent-auth \
  --config /path/ai-session-auth/config.json run -- codex
```

The current working directory and command arguments are preserved.
To use only one provider, set `enabled: false` for the other.

## Verify on your account

Before starting an operational session, open a verification shell:

```bash
./agent-auth --config config.json run -- bash
```

Inside the shell, verify your identity and read-only operations:

```bash
gh api user --jq .login
glab api user
gh pr list --repo YOUR_GITHUB_USERNAME/YOUR_REPOSITORY
glab mr list --repo YOUR_NAMESPACE/YOUR_PROJECT
glab ci list --repo YOUR_NAMESPACE/YOUR_PROJECT
git ls-remote origin
exit
```

This verification creates a real GitLab session PAT and revokes it on exit.
These commands do not print the credentials. Test pushes, comments, PR/MR
creation, and pipeline control on a test repository of your choice.

## Session lifetime and behavior

`session_hours` defaults to 24 and has a maximum of 48. GitLab expiry is rounded
up to midnight UTC at or after the planned end of the session:

| Requested session | GitLab PAT fallback lifetime |
|---|---|
| 24 hours | At least 24 and less than 48 hours |
| 48 hours | At least 48 and less than 72 hours |

Rounding prevents the token from expiring before the session ends.
Normally, the PAT is revoked when the harness exits. If the computer shuts down,
the launcher receives SIGKILL, or revocation fails because of a network error,
the PAT expires on its configured date. At the session limit, the broker stops
issuing credentials and the launcher terminates the harness process group.
Processes that deliberately detach from that group are not guaranteed to stop.

GitHub access tokens last eight hours, with rotating refresh tokens stored in
persistent state. Concurrent sessions for the same App and user share the
current GitHub credential; each wrapper retrieves it afresh. The launcher does
not revoke GitHub credentials on exit, which would invalidate other sessions
or the refresh token. A copied access token remains valid until its actual
expiry or earlier invalidation by GitHub.
A running `gh` command cannot replace its token while it executes; restart
long-running watch commands if they cross a token expiry or rotation.

## Limits and isolation

This launcher manages credential lifecycles; **it is not a sandbox**.
The issuer PAT and refresh token are not passed to the harness. However, an
agent running as your OS user with unrestricted access to your home directory
can read the mode-600 secret files. With the glab issuer, it can likewise run
the real `glab` with your login, by path or without the session's
`GLAB_CONFIG_DIR` and `GITLAB_TOKEN`. Isolation requires a separate OS user,
a sandbox that hides those paths, or an external broker. The session socket
intentionally provides access to the temporary tokens.

The credential helper's allowlist controls where the helper returns tokens;
it does not restrict the CLIs' API calls. GitLab enforces project boundaries
through the token's granular scopes. GitHub enforces the repositories accessible
to the App: `github.repositories` restricts Git credential delivery and does
not further narrow the token's API access. Install the App only on the required
repositories.

The harness must inherit `PATH`, `AI_AUTH_SOCKET`, and `GIT_CONFIG_*`.
A container without the socket mounted, or a shell that clears these variables,
requires additional configuration. Calling `/usr/bin/gh` directly bypasses
the wrapper. CLI configuration directories are temporary; personal aliases,
extensions, and preferences are not copied automatically.

URL rewriting supports `git@github.com:owner/repo.git`,
`ssh://git@github.com/...`, and their GitLab.com equivalents. SSH aliases and
custom hosts or ports are unsupported. Git configuration on disk is unchanged.
Remotes must not contain embedded tokens or passwords.
SSH/GPG commit signing is configured separately.

The included scopes are an initial profile based on provider documentation.
Some `glab` and `gh` commands make additional queries and may require extra
permissions. A 403 never causes automatic privilege expansion.
Verify Git LFS and harnesses with special terminal behavior locally.
A crash during GitHub refresh, after issuance but before saving the rotated
credentials, may require another device authorization.

## Tests

```bash
go test -race ./...
go vet ./...
```

Tests cover identities, UTC expiry, concurrent refresh, cancellable locking,
secret persistence, PAT creation and revocation, the glab issuer through a
fake `glab` (arguments, environment, locking, silent failures), project
matching, repository allowlists, the Git credential protocol, URL rewriting
with real Git, UNIX socket RPC, exit codes, process-group timeouts, and
cancellable issuer input.
Provider APIs are simulated; tests do not use real credentials.
CI runs formatting, vet, race-enabled tests, and builds on Linux and macOS
with stable Go. All 15 tests passed on both platforms, including UNIX socket RPC.

The environment used to build the initial implementation prohibits UNIX socket
creation, so the socket test is skipped there on EPERM/EACCES. The launcher
requires an environment that allows UNIX sockets. Real authenticated provider
flows require the setup above and have not been tested on your accounts.

The GitHub state and configuration formats are compatible with the previous
Python launcher, so existing files can be reused. Keep OAuth state and PATs
out of the repository.

## Technical references

- https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app
- https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens
- https://docs.gitlab.com/auth/tokens/fine_grained_access_tokens/
- https://gitlab.com/gitlab-org/gitlab/-/blob/master/app/graphql/mutations/users/personal_access_tokens/create.rb
- https://docs.gitlab.com/cli/api/
- https://docs.gitlab.com/api/graphql/reference/experimental/input_objects/#personalaccesstokencreateinput
- https://gitlab.com/gitlab-org/gitlab/-/merge_requests/228600
- https://docs.gitlab.com/auth/tokens/fine_grained_access_tokens_rest/
- https://docs.gitlab.com/auth/tokens/fine_grained_access_tokens_other/
- https://git-scm.com/docs/gitcredentials
