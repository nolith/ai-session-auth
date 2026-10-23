# Sessions

`geas run -- COMMAND [ARGS...]` starts a session: it mints credentials,
starts COMMAND as the harness, and revokes what it can when the harness
exits. The working directory and arguments are passed through unchanged.

Inside a session (`GEAS_SESSION_DIR` set), `geas run -- COMMAND` runs
COMMAND in place, sharing the session: it reads no configuration and mints
nothing.

## Session start

The harness never waits for the network. A session starts in this order:

1. **Plan**, without a network call: the configuration, the session
   directory with its gitconfig and sandbox policy, and one marker per
   provider, `pending`.
2. **The reaper** starts, before anything revocable exists.
3. **The harness** starts.
4. **Every provider acquires its credential in the background**, in
   parallel: it writes the credential's files and its tools'
   configuration, then its marker, `ready`, or `failed` with the reason.

Until a provider is ready, its tools see no credential:

| Tool                    | Before the credential arrives                                                                    |
| :---------------------- | :----------------------------------------------------------------------------------------------- |
| Git, through the helper | `geas credential` waits up to 30 s for the provider's marker, then tells Git to quit, with why   |
| `glab`, `gh`            | not logged in; agents retry                                                                      |
| `ssh -F`, the session's | no `ssh/config` yet: the agent asks you to run `geas ssh grant`, which reaches it within seconds |

A GitLab mint takes about 3 s, so the helper's 30 s cover a slow network
ten times over and still fail well inside an agent's tool timeout. A
provider that failed makes the helper answer at once.

## geas status

Run inside a session, `geas status` reads the markers and prints one line
per provider:

```console
$ geas status
github:github.com  failed: run github-login before starting a session
gitlab:gitlab.com  ready
sandbox            failed: readable: /home/alice/.config/glab-cli
```

The `sandbox` line, present only with a `wrapper`, is the background
[sandbox check](security.md#checking-the-sandbox). `geas status` exits 0
when everything is ready, 1 when something failed, and 75 (`EX_TEMPFAIL`,
try again later) while something is still pending and nothing failed.

## Nothing on the terminal

geas prints nothing between the harness's start and its exit: a line would
scribble over the agent's terminal. Failures go to the markers, Git's
credential helper reports them, and `geas status` shows them.

After the harness exits, geas may print the revocation's result, a
revocation the reaper retries, the reason the session stopped the harness,
the providers' timings with `GEAS_DEBUG=timing`, and last the wrapper's
`early_exit_hint`.

`GEAS_DEBUG=timing` prints each step's duration on stderr up to
`harness start`; the providers' steps, prefixed with their ID and timed from
the harness's start, follow once the harness has exited.

## When a provider fails

A provider whose credential cannot be obtained never stops the session.
For GitHub: a missing, unreadable or expired login, a failed refresh or
`expected_user` check. For GitLab: a failed issuer (logged-out or missing
glab, unreadable `token_file`), user check, namespace resolution or PAT
creation.

The provider then stays unavailable for the whole session, and never falls
back to your own login: `GH_CONFIG_DIR` and `GLAB_CONFIG_DIR` still point
into the session directory, where no token arrives, so `gh` and `glab`
report that they are not logged in. Git HTTPS to an allowlisted repository
on that host fails at once, with the helper telling Git to quit with
"<host> is not available in this session: <reason>", rather than asking a
terminal or an askpass program.

Fix the login (`geas github-login`, `glab auth login` or
`geas save-gitlab-issuer`) and start a new session.

## Lifetime and revocation

`session_hours` defaults to 24, at most 48. GitLab PATs expire at midnight
UTC at or after the planned end of the session, so a token never expires
mid-session:

| Requested session | GitLab PAT expiry                  |
| :---------------- | :--------------------------------- |
| 24 hours          | at least 24 and less than 48 hours |
| 48 hours          | at least 48 and less than 72 hours |

That expiry is only the fallback. The PAT is revoked when:

- the harness exits;
- Ctrl-C, SIGTERM, or the SIGHUP of a closed terminal or pane, stops the
  harness's process group. geas revokes before printing anything, survives
  an output that has gone away, and runs glab in its own process group so
  the hangup cannot interrupt the revocation;
- the session time limit is reached: geas stops the harness's process
  group and removes the session directory, with the credentials in it.
  Processes that deliberately detach from that group are not guaranteed to
  stop;
- the launcher is killed outright: the reaper revokes it, see below.

When the harness exits during a GitLab mint, geas waits for the mint's
answer, within glab's or the API's timeout, and revokes the PAT: a mint,
once sent, is never cut short.

If the computer shuts down, or revocation keeps failing on a network
error, the PAT expires on its date.

### The ledger and the reaper

Every PAT is in the session's `ledger` before it exists: geas records the
PAT's name before the request that mints it, and its ID once resolved. At
exit, geas revokes what the ledger lists, and removes the session's
directory once nothing is left. A directory whose ledger still lists
something stays, for the reaper and as evidence.

Some terminals do not wait: closing a multiplexer's pane may follow SIGHUP
and SIGTERM with SIGKILL half a second later. So before acquiring any
credential, geas starts a reaper (`geas _reaper`), detached twice, outside
the pane's process tree. It gets only the configuration path and the
session ID, and waits on a pipe whose write end only the launcher holds:

- after a clean revocation the launcher tells it so, and the reaper exits;
- if the pipe closes otherwise, because the launcher died or its own
  revocation failed, the reaper revokes every PAT the ledger still lists.
  It finds a PAT by name when the launcher died before resolving its ID,
  and checks that the ID still names the session PAT. It retries after 10
  seconds and after a minute, then removes the session's directory once
  nothing is left.

The reaper logs to `$XDG_STATE_HOME/geas/reaper.log`
(`~/.local/state/geas/reaper.log`), never to a terminal.

### GitHub tokens

GitHub access tokens last eight hours, with rotating refresh tokens kept in
`state_file`. Concurrent sessions for the same App and user share the
current credential: shortly before it expires, each session refreshes it in
the state file, under its lock, or picks up the token another session
rotated there, and rewrites its own `gh/hosts.yml` and secret.

A refresh that fails is retried every minute, silently. Once the token has
expired with no replacement, GitHub's marker turns `failed`, so Git's helper
says why, and the next token makes it `ready` again.

geas does not revoke GitHub tokens on exit: that would invalidate other
sessions or the refresh token. A copied access token stays valid until it
expires.

- A running `gh` command cannot pick up a new token: restart long-running
  watch commands that cross a token rotation.
- A crash during a refresh, after GitHub issued the new tokens but before
  geas saved them, may require another `geas github-login`.

## Exit codes

| Code    | When                                                                                         |
| :------ | :------------------------------------------------------------------------------------------- |
| any     | the harness's own exit code, unchanged, once it has started                                  |
| 125     | geas failed before the harness started; the error is on stderr                               |
| 128 + n | the harness was killed by signal n                                                           |
| 130     | Ctrl-C, SIGTERM or SIGHUP interrupted the session; geas stopped the harness's process group  |
| 124     | the session time limit was reached; geas stopped the harness's process group                 |
| 2       | the command line is wrong, before geas knows the command is `run`                            |

125 covers everything geas does before the harness: a missing or invalid
configuration, a session directory or a reaper it cannot create, a harness
or a wrapper it cannot start, and, inside a session, a nested `geas run`
whose `GEAS_SESSION_DIR` is unusable. A launcher around geas can tell from
it that the harness never ran. A harness that itself exits 125 is passed
through as it is. Outside `geas run`, a configuration error exits 1.
