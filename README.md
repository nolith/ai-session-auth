<p align="center">
  <img src="assets/geas.svg" alt="GEAS logo: a gold key bound in a teal Celtic knot" width="160">
</p>

# GEAS: temporary credentials for AI agents, under your own identity

GEAS stands for "GEAS Enforces Agent Scopes". In Celtic myth a geas is a
binding obligation laid on a hero: power, held under rules. Agents started
with `geas` act as you, but only through short-lived credentials scoped to the
repositories you allow.

A Go launcher for GitHub.com and GitLab.com on Linux and macOS (amd64 and arm64).
It has no external Go dependencies. Build with Go 1.23 or later; use the latest
stable release where possible. Install `git`, `gh`, and `glab` in your `PATH`.

```bash
git clone https://github.com/nolith/geas.git
cd geas
go build -o geas .
```

The compiled binary does not require a Go runtime.

With Nix, build or run it from the flake, or enter a shell with Go, `gh`,
`glab`, and the formatter:

```bash
nix build            # ./result/bin/geas
nix run . -- --help
nix develop
```

Start an AI harness with API and Git HTTPS access under your own identity:

```bash
./geas --config config.json run -- codex
```

Replace `codex` with any command and its arguments.
Standard GitHub and GitLab SSH remotes work without unlocking your SSH keyring.

## Features

- GitHub App **user access tokens**, acting as you rather than an installation bot.
- Initial authorization through device flow, without a client secret or RSA key.
- GitHub token refresh on demand, starting five minutes before expiry.
- A new **personal fine-grained GitLab PAT** for each session, issued through
  your own `glab` login or a stored issuer PAT.
- GitLab access restricted to selected groups, your personal projects or single
  projects, without PAT management permissions in the agent token.
- Username verification on both providers before starting the harness.
- `gh` and `glab` wrappers that retrieve the current token and invoke the original CLIs.
- A Git credential helper, `geas credential`, that checks protocol, hostname,
  and repository path, and answers from the session directory.
- SSH-to-HTTPS URL rewriting through a session gitconfig, included last from
  the process environment; your Git configuration on disk is unchanged.
- GitLab PAT revocation on normal exit, Ctrl-C, SIGTERM, SIGHUP (a closed
  terminal or pane), or the session time limit, and by a detached reaper when
  the launcher is killed outright.
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
8. Set where Git receives the token, by path and in any letter case, as
   GitHub compares names; use either list or both:
   - `owners`: accounts or organizations, such as `nolith`, one segment
     each, without wildcards. An owner covers all of its repositories, forks
     included; `nolith` never matches `nolith-foo/...`.
   - `repositories`: single repositories, such as `some-org/project`.

   The App installation remains the token's real boundary.

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
  `CI_JOB_TOKEN`, `GLAB_CONFIG_DIR`, `GEAS_SOCKET`, host overrides or HTTP
  debugging, so it acts with your own login, never with a session's.
- Calls are serialized through `$XDG_STATE_HOME/geas/glab.lock`
  (`~/.local/state/geas/glab.lock` by default). GitLab returns a
  new refresh token on every refresh, so two glab processes refreshing at once
  can race. The lock covers geas's own calls, not yours.
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
- In the allowlisted groups and projects, and your personal projects if
  enabled: at least every permission granted to the agent token, as listed in
  `gitlab.permissions` in `config.example.json`.

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

### Allowlist

List what agents may reach by path, with no numeric IDs:

- `gitlab.groups`: groups such as `gitlab-org` or `gitlab-org/ai`. A group
  covers its subgroups and all their projects.
- `gitlab.personal_projects`: `true` adds the projects in your own namespace,
  `expected_user/...`.
- `gitlab.projects`: single projects, such as `some-group/project`.
- `gitlab.permissions`: one list, granted on all of the above.

At session start, each group and project is looked up with `GET groups/:path`
or `GET projects/:path` and refused unless GitLab reports the same path, in the
same case, so a renamed or mistyped namespace stops the session before any PAT
exists. The session PAT then gets `read_user` on your user (`USER`), the
permissions on the resolved groups and projects (`SELECTED_MEMBERSHIPS`), and
on your personal projects (`PERSONAL_PROJECTS`) if enabled. `ALL_MEMBERSHIPS`
and `INSTANCE` are never requested, and PAT management permissions are refused.

The Git credential helper offers the token only for paths inside these
namespaces, by whole segments: `gitlab-org` covers `gitlab-org/gitlab` and
`gitlab-org/ai/skills`, never `gitlab-org-foo/project`. Paths are
case-sensitive, and `projects` match exactly.

A group grant is wide: with `gitlab-org`, a session can push branches, open
merge requests and run jobs in every project there that your role allows, not
only the one it works on. The included profile covers code, issues and
comments, MR creation and updates, pipeline and job control, and artifact
reads. Destructive, administrative, and merge permissions are excluded from
the initial profile. Git pushes remain subject to your user permissions and
the project's branch protections.

#### Project IDs (earlier format)

Configurations with exact `repositories`, numeric `project_ids` and
hand-written `granular_scopes` keep working, but cannot be mixed with the
fields above:

- `gitlab.repositories`: exact paths, such as `nolith/project`;
- `gitlab.project_ids`: numeric project IDs;
- `gitlab.granular_scopes`: `USER` with `read_user`, and `SELECTED_MEMBERSHIPS`
  with `resourceIds` such as `gid://gitlab/Project/ID`.

The launcher queries the projects and verifies that their IDs and paths match.

## Configuration and launch

```bash
cp config.example.json config.json
# Set the Client ID, usernames, GitHub owners or repositories and GitLab groups before proceeding.
./geas --config config.json save-gitlab-issuer   # not with issuer "glab"
./geas --config config.json github-login
./geas --config config.json run -- codex
```

`save-gitlab-issuer` prompts for the PAT with hidden input. Do not put it in the
command line or configuration file. It saves the PAT at `issuer_token_file`
with mode 600. `github-login` displays a URL and a one-time code to authorize
in your browser; it does not print the returned tokens. It saves the OAuth
state at `state_file`.

Use absolute paths to launch the harness from another checkout:

```bash
/path/geas/geas \
  --config /path/geas/config.json run -- codex
```

The current working directory and command arguments are preserved.
To use only one provider, set `enabled: false` for the other.

A provider with `optional: true` (default `false`) does not stop the session
when its credential cannot be obtained at start: a missing, unreadable or
expired GitHub login, a failed refresh or `expected_user` check, or for GitLab
a failed issuer (logged-out glab, unreadable `issuer_token_file`), user check,
group or project resolution or PAT creation. The launcher prints one warning
per skipped provider, with the reason and the fix (`geas github-login`,
`glab auth login` or `geas save-gitlab-issuer`), and starts the harness
even if every enabled provider was skipped. Configuration errors, a missing
`gh` or `glab`, and an interrupted start still stop it. The example marks
GitHub optional, so a lapsed GitHub login does not keep agents from starting
with GitLab.

## Verify on your account

Before starting an operational session, open a verification shell:

```bash
./geas --config config.json run -- bash
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

Rounding prevents the token from expiring before the session ends. Normally,
the PAT is revoked when the harness exits. Ctrl-C, SIGTERM and the SIGHUP of a
closed terminal or pane stop the harness process group and revoke it too; the
launcher revokes before printing anything, survives an output that has gone
away, and runs glab in its own process group so that the hangup cannot
interrupt a revocation.

Each session has a directory of its own,
`$XDG_STATE_HOME/geas/sessions/<session id>`
(`~/.local/state/geas/sessions/` by default), owner-only (mode 700), whose
files are replaced atomically. Every PAT is in the session's ledger there
before it exists: the `ledger` file records the PAT's name before the
request that mints it and its ID once it is resolved. The launcher revokes
what the ledger lists, and removes the session's directory once nothing is
left; a directory whose ledger still lists something stays, for the reaper
and as evidence.

Some terminals do not wait: closing a herdr pane follows SIGHUP and SIGTERM
with SIGKILL about half a second later. Before acquiring any credential, the launcher therefore
starts a reaper (`geas _reaper`) in a new session, detached twice so
that it is outside the pane's process tree. It receives only the configuration
path and the session ID, and waits on a pipe whose write end only the
launcher holds. After a clean revocation the launcher tells it so, and the
reaper exits. If the pipe closes otherwise, because the launcher died by any
signal or its own revocation failed, the reaper revokes every PAT the ledger
still lists through the issuer: it finds the PAT by name when the launcher died
before resolving its ID, and checks that the ID still names the session PAT.
It retries after 10 seconds and a minute if needed, removes the session's
directory once nothing is left (also for a session that minted nothing), and logs what it did to
`$XDG_STATE_HOME/geas/reaper.log`
(`~/.local/state/geas/reaper.log` by default), never to a terminal.

If the computer shuts down, both processes are killed, or revocation keeps
failing because of a network error, the PAT expires on its configured date.
At the session limit, the broker stops issuing credentials and the launcher
terminates the harness process group. Processes that deliberately detach from
that group are not guaranteed to stop.

GitHub access tokens last eight hours, with rotating refresh tokens stored in
persistent state. Concurrent sessions for the same App and user share the
current GitHub credential; each wrapper retrieves it afresh. The launcher does
not revoke GitHub credentials on exit, which would invalidate other sessions
or the refresh token. A copied access token remains valid until its actual
expiry or earlier invalidation by GitHub.
A running `gh` command cannot replace its token while it executes; restart
long-running watch commands if they cross a token expiry or rotation.

A skipped optional provider stays unavailable for the whole session, and never
falls back to your own login: its wrapper, CLI configuration directory in
the session and credential helper remain in place. `gh` or `glab` then fails with
"GitHub is not available in this session: <reason>" (or GitLab) without
running the real CLI, and Git HTTPS to an allowlisted repository on that host
fails at once, with the helper telling Git to quit rather than ask a terminal
or askpass program. A skipped GitLab mints no PAT, so its ledger stays empty
and there is nothing to revoke at exit. Start a new session after fixing the login.

## Limits and isolation

This launcher manages credential lifecycles; **it is not a sandbox**.
The issuer PAT and refresh token are not passed to the harness. However, an
agent running as your OS user with unrestricted access to your home directory
can read the mode-600 secret files. With the glab issuer, it can likewise run
the real `glab` with your login, by path or without the session's
`GLAB_CONFIG_DIR` and `GITLAB_TOKEN`. Isolation requires a separate OS user,
a sandbox that hides those paths, or an external broker. The session socket
intentionally provides access to the temporary tokens.

Each session writes the paths a sandbox around the harness must protect into
its directory: `sandbox/policy.json` lists them neutrally, `deny` (your glab
and gh logins, a stored issuer PAT, the GitHub App state and its lock, and
`$XDG_STATE_HOME/geas` as a whole), `read` and `reopen` (the session's own
directory, inside the denied state directory), and `sandbox/nono.json` is the
same as a [nono](https://github.com/nolabs-ai/nono) profile fragment with
`filesystem.deny`, `read` and `bypass_protection`, and no `extends`: the
launcher that runs nono names its own base profile. `GEAS_SANDBOX_PROFILE`
holds the fragment's path. geas resolves each tool's directory as the tool
does: `GLAB_CONFIG_DIR`, else `$XDG_CONFIG_HOME/glab-cli`, else the OS's user
configuration directory (`~/Library/Application Support/glab-cli` on
macOS); `GH_CONFIG_DIR`, else `$XDG_CONFIG_HOME/gh`, else `~/.config/gh`.

The credential helper's allowlist controls where the helper returns tokens;
it does not restrict the CLIs' API calls. GitLab enforces the group and
project boundaries through the token's granular scopes. GitHub enforces the
repositories accessible to the App: `github.owners` and `github.repositories`
restrict Git credential delivery and do not further narrow the token's API
access.
Install the App only on the required repositories.

The harness must inherit `PATH`, `GEAS_SOCKET`, and `GIT_CONFIG_*`. A
container without the socket mounted, or a shell that clears these variables,
requires additional configuration. Calling `/usr/bin/gh` directly bypasses the
wrapper. The real `gh` and `glab` run with the session's wrapper directory
removed from `PATH`, so a version-manager shim, such as mise's, recorded as the
real CLI and running the next one on `PATH` cannot loop back to the wrapper. A
shim that calls the wrapper by its path fails with a "wrapper loop" error
instead of spinning. `glab` and `gh` read their configuration from the
session directory (`GLAB_CONFIG_DIR` and `GH_CONFIG_DIR` point there), where
geas writes `glab/config.yml` and `gh/hosts.yml` with the session's
credentials; personal aliases, extensions, and preferences are not copied.

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
nix fmt              # gofmt and nixfmt through treefmt
treefmt              # the same, from treefmt.toml (inside nix develop)
nix flake check      # formatting, vet, race-enabled tests and the build
```

Tests cover identities, UTC expiry, concurrent refresh, cancellable locking,
secret persistence, PAT creation and revocation, the glab issuer through a
fake `glab` (arguments, environment, locking, silent failures), group and
project resolution, scope building, namespace, owner and repository allowlists, the
Git credential protocol, PATH shims and the wrapper loop guard, URL rewriting
with real Git, UNIX socket RPC, exit codes, process-group timeouts, revocation
after SIGHUP with the output gone, the ledger, the reaper after SIGKILL (also
during the mint, finding the PAT by name) and its silence after a clean exit, cancellable issuer input, and optional providers skipped
with warnings while their wrappers and Git refuse, or stopping the session when
required.
Provider APIs are simulated; tests do not use real credentials.
CI runs `nix flake check` on Linux and macOS with Go from nixpkgs-unstable:
formatting, vet, race-enabled tests, and the build. All 15 tests passed on
both platforms, including UNIX socket RPC.

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
