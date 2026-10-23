# Inside a session

The harness, and everything it runs, gets its credentials from the session
directory, through the environment geas sets. The real `git`, `gh`, `glab`
and `ssh` run from wherever `PATH` finds them, version-manager shims
included.

## The environment

The harness inherits geas's environment without `GH_TOKEN`,
`GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN`, `GITHUB_ENTERPRISE_TOKEN`,
`GITLAB_TOKEN`, `GITLAB_ACCESS_TOKEN`, `OAUTH_TOKEN`, `CI_JOB_TOKEN`,
`SSH_AUTH_SOCK`, `GH_DEBUG`, `GLAB_DEBUG` and `GLAB_DEBUG_HTTP`, and with:

| Variable                                 | Example                                                                         | Set by      |
| :--------------------------------------- | :------------------------------------------------------------------------------ | :---------- |
| `GEAS_SESSION_DIR`                       | `~/.local/state/geas/sessions/20261023T001500Z-1f0c2a9b3e4d5c6a`                | the session |
| `GEAS_SANDBOX_PROFILE`                   | `$GEAS_SESSION_DIR/sandbox/nono.json`                                           | the session |
| `GIT_CONFIG_COUNT`, `_KEY_n`, `_VALUE_n` | one more entry, `include.path=$GEAS_SESSION_DIR/gitconfig`, after any inherited | the session |
| `GIT_TERMINAL_PROMPT`                    | `0`                                                                             | the session |
| `GLAB_CONFIG_DIR`                        | `$GEAS_SESSION_DIR/glab`                                                        | GitLab      |
| `GH_CONFIG_DIR`                          | `$GEAS_SESSION_DIR/gh`                                                          | GitHub      |
| `GH_PROMPT_DISABLED`                     | `1`                                                                             | GitHub      |
| `AGENT_SSH_NAME`                         | `claude-myproject`, the agent's SSH name                                        | SSH         |

`PATH` is unchanged. The Nix package puts `ssh-keygen`, `ykman` and
`fido2-token` first on geas's own `PATH`, and saves yours first in
`GEAS_UNWRAPPED_PATH`, which the harness gets back.

Two providers may set one variable only to the same value, and none may set
one of the session's own; anything else stops the session before it starts.

The harness must keep `GEAS_SESSION_DIR`, `GLAB_CONFIG_DIR`,
`GH_CONFIG_DIR` and `GIT_CONFIG_*`, and be able to read the session
directory. A container without that directory mounted, or a shell that
clears these variables, needs extra configuration.

## Git

The session's gitconfig, included last through `GIT_CONFIG_*`, sets
`geas credential` as Git's credential helper and rewrites SSH remotes to
HTTPS. Your Git configuration on disk is unchanged.

- The helper checks protocol, host and repository path against the
  allowlists, and answers from the session directory, waiting briefly for a
  credential still on its way.
- Rewriting covers `git@github.com:owner/repo.git`,
  `ssh://git@github.com/...`, and the same forms on each GitLab host. SSH
  aliases and custom ports are not supported.
- Remotes must not contain embedded tokens or passwords.
- SSH or GPG commit signing is configured separately.

## gh and glab

`GH_CONFIG_DIR` and `GLAB_CONFIG_DIR` point into the session directory,
where geas writes `gh/hosts.yml` and `glab/config.yml` with the session's
credentials. Your personal aliases, extensions and preferences are not
copied.

## The session directory

Each session has a directory of its own,
`$XDG_STATE_HOME/geas/sessions/<session id>`
(`~/.local/state/geas/sessions/` by default). It is owner-only, mode 700,
every file mode 600, each replaced atomically.

| File                                        | Written by                                                                                 | Read by                             |
| :------------------------------------------ | :----------------------------------------------------------------------------------------- | :---------------------------------- |
| `ledger`                                    | the launcher, before each mint; the reaper                                                 | the launcher and the reaper         |
| `gitconfig`                                 | the launcher, at start                                                                     | Git, through `include.path`         |
| `git/hosts.json`                            | the launcher, at start: hosts and allowlists                                               | `geas credential`                   |
| `secrets/<provider>`                        | the launcher, when a credential arrives or is refreshed; not for SSH                       | `geas credential`                   |
| `markers/<provider>`                        | the launcher: `pending`, then `ready` or `failed` with the reason                          | `geas credential`, `geas status`    |
| `markers/sandbox`                           | the launcher, with a wrapper: the background sandbox check's verdict                       | `geas status`                       |
| `glab/config.yml`                           | the GitLab renderer, one entry per host                                                    | `glab`                              |
| `gh/hosts.yml`, `gh/config.yml`             | the GitHub renderer                                                                        | `gh`                                |
| `ssh/id_ed25519`, `ssh/id_ed25519-cert.pub` | the SSH provider: the agent's key and certificate, copied; removed once expired or dropped | `ssh`                               |
| `ssh/config`                                | the SSH renderer, with the key and certificate; removed with them                          | `ssh -F`                            |
| `sandbox/policy.json`                       | the launcher, at start                                                                     | `geas sandbox check`                |
| `sandbox/nono.json`                         | the launcher, at start                                                                     | the sandbox, `GEAS_SANDBOX_PROFILE` |

`<provider>` is a provider's ID, such as `gitlab:gitlab.com`. `glab` and
`gh` may write their own files into their directories; a renderer replaces
its files whole.

The golden files under `testdata/` follow what `glab` 1.123 and `gh` 2.102
write themselves; both read them back unchanged.
