# GitLab setup

Each GitLab instance is one `gitlab` provider, with its own `host` (such as
`gitlab.com` or `ops.gitlab.net`), issuer, user and allowlist. Every session
mints a new **fine-grained personal access token** (PAT) on each instance,
in parallel, and revokes it when the session ends.

```json
{
  "type": "gitlab",
  "host": "gitlab.com",
  "issuer": { "type": "glab" },
  "expected_user": "YOUR_GITLAB_USERNAME",
  "namespaces": ["YOUR_GROUP", "YOUR_GITLAB_USERNAME", "SOME_GROUP/YOUR_PROJECT"],
  "permissions": ["read_repository", "push_code", "..."]
}
```

`host` is a host name in lower case, without scheme or port. The examples
below use gitlab.com; replace it with each instance's host.

## The issuer

The **issuer** is the credential that creates, finds and revokes each
session's PAT. The agent never sees it. It is either your own `glab` login
or a PAT you store.

### Your glab login (recommended)

If `glab auth login --hostname gitlab.com` works, set
`"issuer": {"type": "glab"}`. Nothing is stored and nothing needs rotating:
an OAuth login renews its own two-hour token, so revocation still works at
the end of a long session.

- The login needs the `api` scope, which `glab auth login` requests.
  GitLab checks a new PAT against its creator's permissions only when the
  creator is itself fine-grained, so a legacy-scoped OAuth login can
  create it.
- Every request runs `glab api --hostname <host> ...`, with glab found on
  your `PATH` at session start, run from `/` and pinned to the provider's
  host: neither the current repository's remote nor glab's placeholders can
  redirect it.
- glab runs without `GITLAB_TOKEN`, `GITLAB_ACCESS_TOKEN`, `OAUTH_TOKEN`,
  `CI_JOB_TOKEN`, `GLAB_CONFIG_DIR`, host overrides or HTTP debugging, so it
  always acts with your own login, never with a session's.
- Calls are serialized through one lock for every host and session,
  `$XDG_STATE_HOME/geas/glab.lock` (`~/.local/state/geas/glab.lock`):
  GitLab rotates the refresh token on every refresh, and glab keeps every
  host's login in one file, so two refreshing glab processes could race.
  The lock covers geas's own calls, not yours.
- Each call gets one minute, so a locked keyring fails the request instead
  of stalling the session.
- glab's output is never printed. A failure reports only "glab issuer
  request failed"; run the same `glab api` command yourself to see why.

If the login breaks (`glab auth logout`, a failed refresh, a revoked
authorization), GitLab fails in every new session and running sessions
cannot revoke their PATs, which then expire on their own. `glab auth login`
fixes both.

### A stored PAT

Set `"issuer": {"type": "bearer", "token_file":
"~/.local/state/geas/gitlab-issuer"}`. geas sends that PAT as a bearer token
to `https://<host>`.

Create the issuer PAT for your own user at
<https://gitlab.com/-/user_settings/personal_access_tokens>, separate from
the agents' session PATs. Prefer a fine-grained one with:

- in the **User** boundary: Personal Access Token **Create, Read, Revoke**,
  and User **Read** to verify your identity:

  ```json
  ["create_personal_access_token", "read_personal_access_token", "revoke_personal_access_token", "read_user"]
  ```

- in the allowlisted namespaces: at least every permission the session PAT
  gets, as listed in `permissions`.

A fine-grained issuer can create PATs only with permissions and boundaries
equal to or narrower than its own, so Create alone cannot issue a
push-capable token; Read and Revoke are needed for cleanup. A legacy PAT
with the `api` scope also works. Either way, the session PAT is
fine-grained.

Store it with hidden input, never on the command line or in the
configuration:

```bash
geas save-gitlab-issuer               # --host HOST with several bearer issuers
```

It writes the PAT to `token_file`, mode 600. geas does not renew the issuer
PAT: it remains subject to its own expiry and the instance's policies.

## The allowlist

List what agents may reach by path, never by numeric ID:

- `namespaces`:
  - groups, such as `gitlab-org` or `gitlab-org/ai`, which cover their
    subgroups and all their projects;
  - your own namespace, `expected_user`, which covers your personal
    projects;
  - single projects, such as `some-group/project`.
- `permissions`: one list, granted on all of them. `config.example.json`
  has a profile for code, issues and comments, merge request creation and
  updates, pipeline and job control, and artifact reads. It leaves out
  destructive, administrative and merge permissions.

At session start, one GraphQL query checks your user and resolves every
path; the global ID's model says what each path is:

| Model                          | Granted as                                          |
| :----------------------------- | :-------------------------------------------------- |
| `Group`                        | `SELECTED_MEMBERSHIPS`, the group's ID              |
| `Namespaces::UserNamespace`    | `PERSONAL_PROJECTS`, only for `expected_user`'s own |
| `Namespaces::ProjectNamespace` | `SELECTED_MEMBERSHIPS`, the project's ID            |

The session PAT also gets `read_user` on your user (`USER`). Refused,
before any PAT exists:

- a path that does not resolve, or that GitLab reports with another path or
  in another case, such as a renamed or mistyped namespace;
- another user's namespace;
- PAT management permissions. `ALL_MEMBERSHIPS` and `INSTANCE` are never
  requested.

Git's credential helper offers the token only for paths inside these
namespaces, by whole segments, or for a project's own path: `gitlab-org`
covers `gitlab-org/gitlab` and `gitlab-org/ai/skills`, never
`gitlab-org-foo/project`. Paths are case-sensitive.

> [!WARNING]
> A group grant is wide. With `gitlab-org`, a session can push branches,
> open merge requests and run jobs in every project there that your role
> allows, not only the one it works on. Pushes remain subject to your own
> permissions and to branch protections.

## References

- [Fine-grained personal access tokens](https://docs.gitlab.com/auth/tokens/fine_grained_access_tokens/)
- [Fine-grained tokens: REST API](https://docs.gitlab.com/auth/tokens/fine_grained_access_tokens_rest/)
- [Fine-grained tokens: other endpoints](https://docs.gitlab.com/auth/tokens/fine_grained_access_tokens_other/)
- [`PersonalAccessTokenCreateInput`](https://docs.gitlab.com/api/graphql/reference/experimental/input_objects/#personalaccesstokencreateinput)
- [The PAT create mutation](https://gitlab.com/gitlab-org/gitlab/-/blob/master/app/graphql/mutations/users/personal_access_tokens/create.rb)
  and [its merge request](https://gitlab.com/gitlab-org/gitlab/-/merge_requests/228600)
- [`glab api`](https://docs.gitlab.com/cli/api/)
