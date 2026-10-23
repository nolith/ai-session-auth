# GitHub setup

geas gets GitHub.com access through a GitHub App's **user access tokens**:
the token acts as you, not as an installation bot, and can do only what both
you and the App are allowed to do. The first authorization uses the device
flow, so there is no client secret, RSA key or public service to run.

## Register the App, once

1. Open <https://github.com/settings/apps> and register a GitHub App.
2. Disable webhooks; geas does not use them.
3. Enable **Device flow** and keep user access token expiration enabled.
4. Grant these repository permissions:

   | Permission      | Level                |
   | :-------------- | :------------------- |
   | Contents        | Read and write       |
   | Issues          | Read and write       |
   | Pull requests   | Read and write       |
   | Actions         | Read and write       |
   | Checks          | Read-only            |
   | Commit statuses | Read-only            |
   | Metadata        | Read-only, automatic |

   To edit `.github/workflows/` files, also enable **Workflows: write**.
   Leave Administration, Secrets and protection bypass disabled.
5. Install the App on your account or organization, selecting only the
   repositories agents need. Organizations may require administrator
   approval and an active SSO session.

## Configure the provider

```json
{
  "type": "github",
  "host": "github.com",
  "client_id": "Iv23_REPLACE_WITH_YOUR_APP_CLIENT_ID",
  "expected_user": "YOUR_GITHUB_USERNAME",
  "state_file": "~/.local/state/geas/github.json",
  "owners": ["YOUR_GITHUB_USERNAME"],
  "repositories": ["SOME_ORG/YOUR_REPOSITORY"]
}
```

| Field           | Meaning                                                                 |
| :-------------- | :---------------------------------------------------------------------- |
| `host`          | `github.com`, the only GitHub host supported                            |
| `client_id`     | the App's **Client ID**, not its App ID                                 |
| `expected_user` | your GitHub username; a token for anyone else is never handed over      |
| `state_file`    | where `geas github-login` keeps the OAuth state, refresh token included |
| `owners`        | accounts or organizations whose repositories Git may push to            |
| `repositories`  | single repositories, `owner/repo`                                       |

Use `owners`, `repositories` or both. Names are compared by path and in any
letter case, as GitHub does. An owner is one segment, without wildcards, and
covers all of its repositories, forks included: `nolith` never matches
`nolith-foo/...`.

These lists decide where Git's credential helper hands out the token; they
do not narrow the token's API access. The App installation is the token's
real boundary, so install it only on the repositories you need.

Then authorize once:

```bash
geas github-login
```

It prints a URL and a one-time code to enter in your browser, and saves the
OAuth state at `state_file`, mode 600. It never prints the tokens.

## Tokens over time

GitHub access tokens last eight hours, with rotating refresh tokens. geas
refreshes the token shortly before it expires, in the background; see
[Sessions](sessions.md#github-tokens) for how concurrent sessions share it.

Commit authorship still comes from `git config user.name` and
`user.email`: the token identifies who pushes, it does not rewrite the
author.

## References

- [Generating a user access token for a GitHub App](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app)
- [Refreshing user access tokens](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens)
- [Git credential storage](https://git-scm.com/docs/gitcredentials)
