# Configuration

The configuration is `$XDG_CONFIG_HOME/geas/config.json`, else
`~/.config/geas/config.json`, on macOS too. `--config PATH`, before the
command, names another:

```bash
geas --config ~/work/geas.json run -- opencode
```

Start from [`config.example.json`](../config.example.json). The file is a
list of providers:

```json
{
  "version": 2,
  "session_hours": 24,
  "providers": [
    { "type": "gitlab", "host": "gitlab.com", "issuer": { "type": "glab" }, "expected_user": "...", "namespaces": ["..."], "permissions": ["..."] },
    { "type": "github", "host": "github.com", "client_id": "...", "expected_user": "...", "state_file": "...", "owners": ["..."] },
    { "type": "ssh" }
  ]
}
```

| Field           | Meaning                                                                         |
| :-------------- | :------------------------------------------------------------------------------ |
| `version`       | `2`, required                                                                   |
| `session_hours` | the session's length, 24 by default, at most 48                                 |
| `wrapper`       | optional: the command the harness starts through, see [below](#the-wrapper)     |
| `no_new_privs`  | Linux only: `true` sets no_new_privs on geas, see [below](#no_new_privs)        |
| `providers`     | one entry per credential source, by `type`: `gitlab`, `github` or `ssh`         |

The providers:

- `gitlab`: one per instance, see [GitLab setup](gitlab.md).
- `github`: `github.com` only, see [GitHub setup](github.md).
- `ssh`: at most one, with no other field; it hands the agent the
  certificate you grant with `geas ssh grant`, see
  [SSH access for agents](ssh.md).

Each host has at most one provider. Relative paths are relative to the
configuration's directory, and `~/` is your home. Unknown fields are
refused, with their name. To use a single provider, leave the others out.

## Checking the configuration

`geas config check` loads the configuration, refuses it as `geas run`
would, and lists its providers:

```console
$ geas config check
/home/alice/.config/geas/config.json: version 2, session_hours 24
  gitlab:gitlab.com  issuer glab, as alice
  github:github.com  as alice
  ssh  the agent's certificate from geas ssh grant
```

`geas allowed URL` tells whether a session's Git would get a credential for
a remote. It reads only the configuration, with no network and outside a
session too, which makes it a good preflight before cloning:

```console
$ geas allowed git@gitlab.com:gitlab-org/gitlab.git
gitlab:gitlab.com
$ geas allowed https://gitlab.com/someone-else/project; echo $?
1
```

It exits 0 and prints the provider's ID when a provider for the remote's
host covers the path, and 1 when not. It takes the forms a session hands to
the credential helper: `https://HOST/PATH`, and `git@HOST:PATH` and
`ssh://git@HOST/PATH`, which the session rewrites to HTTPS. Any other form
exits 1 with an error. For GitLab, it checks the configured paths only, as
it cannot know yet which namespaces are projects.

## The wrapper

With a `wrapper`, such as a sandbox's launcher, geas starts the harness as
the wrapper's `command` followed by the harness's own command line:

```json
"wrapper": {
  "command": ["/usr/local/bin/agent-sandbox"],
  "early_exit_hint": "skip the sandbox with AGENT_SANDBOX=off, see why with AGENT_SANDBOX_DEBUG=1"
}
```

- The program is an absolute path, a `~/` path or a name found on `PATH`.
- The wrapper runs with the session's environment already set, including
  `GEAS_SESSION_DIR` and `GEAS_SANDBOX_PROFILE`, so it can grant the
  session's directory and apply its profile.
- Every session also runs `geas sandbox check` through the wrapper, in the
  background; see [Security and isolation](security.md#checking-the-sandbox).
- When the harness exits non-zero by itself within 15 seconds of its start,
  geas prints `early_exit_hint` after everything else: a wrapper that
  refuses to start the harness looks like that. The hint is never printed
  while the harness runs, after a clean exit, or when geas itself stopped
  the harness.
- A wrapper that cannot be started is geas's failure, exit code 125.

## no_new_privs

On Linux, `"no_new_privs": true` sets the no_new_privs flag on geas itself,
on every thread, before anything runs, as `setpriv --no-new-privs` would.
The reaper, glab, the wrapper and the harness inherit it, so nothing they
run can gain privileges through a setuid or file-capability program, `sudo`
included.

geas refuses to start (exit code 125) when it cannot set the flag, as in a
binary built with cgo: build with `CGO_ENABLED=0`, as the flake does. On
macOS the field is a configuration error.

## Logins

| Command                   | When                                                   |
| :------------------------ | :----------------------------------------------------- |
| `geas github-login`       | once, and whenever the GitHub refresh token is lost    |
| `geas save-gitlab-issuer` | for each `bearer` issuer, and when its PAT is replaced |
| `glab auth login`         | for each `glab` issuer, outside geas                   |

A provider whose credential cannot be obtained never stops the session; see
[Sessions](sessions.md#when-a-provider-fails). Configuration errors do: they
stop `geas run` before the harness starts, with exit code 125.
