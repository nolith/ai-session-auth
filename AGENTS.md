# geas

geas starts a session: it mints short-lived credentials for a harness (an
agent), hands them to the tools that already read them, and revokes them when
the session ends. Go module `github.com/nolith/geas`, binary `geas`, no
dependencies outside the standard library.

## Where things live

    cmd/geas/                  CLI dispatch, and the launcher's end-to-end tests
    internal/config/           the configuration: schema, validation, path expansion
    internal/session/          one session's lifecycle: plan, start, acquire, teardown,
                               and the reaper process
    internal/api/              the HTTPS client every provider request goes through
    internal/secretfile/       owner-only files: atomic writes, locks, the state directory
    internal/timing/           the GEAS_DEBUG=timing step log, cancellable waits
    internal/procenv/          the environment as a map, shared by the session and glab
    internal/ledger/           revocable credentials on disk, revoked across providers
    internal/provider/         the Provider interface, shared types
    internal/provider/gitlab/  one GitLab instance: its issuers, PATs, glab config
    internal/provider/github/  GitHub App tokens: state file, refresh, gh config
    internal/testutil/         test fixtures, fakes and the fake glab/gh scripts

Later phases add the rest of the end state:

    internal/provider/         the Renderer interface
    internal/sessiondir/       the session directory, atomic writes, markers, gitconfig
    internal/credhelper/       `geas credential`, Git's credential helper
    internal/provider/sshcert/ certificates from `geas ssh grant`, the ssh config
    internal/sshgrant/         the human side: grant, show, drop, enroll, list-cas
    internal/sandbox/          the sandbox policy a provider declares, `geas sandbox check`
    internal/sandbox/nono/     renders that policy as a nono profile fragment
    nix/ scripts/ skills/      packaging and skills, with the ssh work

## Rules

- Providers never import `internal/session`, and nothing below `cmd/geas`
  imports it either: the session builds the providers and hands them what
  they need. A provider kind is one package under `internal/provider`, and a
  new host is configuration, never code.
- Providers declare the paths a sandbox must protect, in the shared types;
  they never import nono or know how that policy is rendered.
- Only what another package needs is exported, and shared types carry only
  what several providers or the session itself act on.
- The binary, the flags, the output, the configuration format and the files
  on disk are the contract. Behaviour changes are their own change.
- Nothing prints to the terminal once the harness owns it: a background
  warning would scribble over the agent's TUI. Failures go to markers, to
  the reaper's log, and to `geas status`.
- Credentials land, atomically, in the files `glab`, `gh`, git and ssh
  already read. Nothing is hidden from a process running as the same user;
  that is the sandbox's job.

## Checks

    gofmt -l .          # must print nothing
    go vet ./...
    go test -race ./...

    nix flake check -L  # authoritative: formatting, vet, race tests, build

    go list -deps ./internal/provider/... | grep internal/session   # must not match

Some launcher and reaper tests run `ps` to check process groups, so they fail
where `ps` cannot run, such as inside a sandbox; `nix flake check` provides
it. Do not tune those tests for a particular environment.
