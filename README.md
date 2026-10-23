<p align="center">
  <img src="assets/geas.svg" alt="GEAS logo: a gold key bound in a teal Celtic knot" width="160">
</p>

# GEAS: temporary credentials for AI agents, under your own identity

GEAS stands for "GEAS Enforces Agent Scopes". In Celtic myth a geas is a
binding obligation laid on a hero: power, held under rules. Agents started
with `geas` act as you, but only through short-lived credentials scoped to
the repositories you allow.

```bash
geas run -- opencode
```

`geas` starts the harness (here `opencode`, but any command works) at once,
then mints credentials for it in the background:

- **GitHub.com**: a GitHub App user access token, acting as you, refreshed
  before it expires.
- **GitLab** (GitLab.com, self-managed, Dedicated, any number of instances):
  a new fine-grained personal access token per session, limited to the
  groups and projects you list, revoked when the session ends.
- **SSH hosts**: a short-lived certificate you sign with a YubiKey, for the
  hosts you name (`geas ssh grant`).

The credentials land in the files `git`, `gh`, `glab` and `ssh` already
read, inside a per-session directory. The real tools run, with no wrappers
and no `PATH` changes. Git SSH remotes are rewritten to HTTPS for the
session, so your SSH keyring stays locked, and your Git configuration on
disk is unchanged.

> [!IMPORTANT]
> geas manages credential lifecycles; **it is not a sandbox**. An agent
> running as your OS user can still read your own `gh` and `glab` logins.
> Every session writes the paths a sandbox must protect, as a neutral policy
> and a [nono](https://github.com/nolabs-ai/nono) profile fragment. See
> [Security and isolation](docs/security.md).

## Install

geas is a single Go binary with no dependencies outside the standard
library, for Linux and macOS (amd64 and arm64). It needs `git`; `gh` and
`glab` are used from your `PATH` when present.

With Nix:

```bash
nix build            # ./result/bin/geas
nix run . -- --help
```

With Go 1.23 or later:

```bash
CGO_ENABLED=0 go build -o geas ./cmd/geas
```

`CGO_ENABLED=0` matters on Linux: `no_new_privs` needs a binary without cgo.
The Nix package also puts OpenSSH, `ykman` and `fido2-token` on geas's own
`PATH`, for `geas ssh`.

## Quick start

1. Set up the providers you want, once:
   [GitHub](docs/github.md) (register a GitHub App) and
   [GitLab](docs/gitlab.md) (pick an issuer and an allowlist).
2. Write the configuration, `~/.config/geas/config.json` by default:

   ```bash
   mkdir -p ~/.config/geas
   cp config.example.json ~/.config/geas/config.json
   $EDITOR ~/.config/geas/config.json   # usernames, Client ID, allowlists
   geas config check
   ```

3. Log in, once:

   ```bash
   geas github-login           # GitHub device flow
   geas save-gitlab-issuer     # only for a GitLab "bearer" issuer
   ```

4. Try a session in a shell, then exit to revoke its GitLab PAT:

   ```bash
   geas run -- bash
   geas status                 # each provider: pending, ready or failed
   gh api user --jq .login
   glab api user
   git ls-remote origin
   exit
   ```

5. Start your agent: `geas run -- opencode`.

A provider that cannot get its credential never stops the session: its
tools report that they are not logged in, Git fails at once with the
reason, and `geas status` says what to fix.

## Documentation

| Topic                                      | What is in it                                                   |
| :----------------------------------------- | :-------------------------------------------------------------- |
| [GitHub setup](docs/github.md)             | The GitHub App, its permissions, the owner and repository lists |
| [GitLab setup](docs/gitlab.md)             | Issuers (your glab login or a stored PAT), the allowlist        |
| [Configuration](docs/configuration.md)     | Every field, `config check`, `allowed`, the wrapper             |
| [Sessions](docs/sessions.md)               | Start order, lifetime, revocation, status, exit codes           |
| [Inside a session](docs/session-files.md)  | The harness's environment and the session directory             |
| [SSH access for agents](docs/ssh.md)       | `geas ssh`, YubiKey agent CAs, host setup                       |
| [Security and isolation](docs/security.md) | What geas protects, what a sandbox must do, `sandbox check`     |
| [Development](docs/development.md)         | Layout, checks and what the tests cover                         |

## Development

```bash
nix develop          # Go, gopls, gh, glab and the formatters
nix fmt              # gofmt, nixfmt and rumdl through treefmt
nix flake check -L   # formatting, vet, race-enabled tests and the build
```

See [Development](docs/development.md) for the plain Go commands and the
test suite.
