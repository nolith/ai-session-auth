package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/nolith/geas/internal/api"
	"github.com/nolith/geas/internal/config"
	"github.com/nolith/geas/internal/credhelper"
	"github.com/nolith/geas/internal/provider/github"
	"github.com/nolith/geas/internal/sandbox"
	"github.com/nolith/geas/internal/secretfile"
	"github.com/nolith/geas/internal/session"
	"github.com/nolith/geas/internal/sessiondir"
	"github.com/nolith/geas/internal/sshgrant"
	"github.com/nolith/geas/internal/timing"
)

func usage(output io.Writer) {
	fmt.Fprintln(output, `Usage: geas [--config config.json] COMMAND

The configuration defaults to $XDG_CONFIG_HOME/geas/config.json, else
~/.config/geas/config.json.

Commands:
  github-login                  Authorize your GitHub App once via device flow
  save-gitlab-issuer [--host HOST] [--stdin]
                                Store the issuer PAT of a GitLab host whose issuer
                                is "bearer", with mode 600
  config check                  Check the configuration and list its providers
  allowed URL                   Exit 0 when a session hands its credentials to Git
                                for the remote URL (HTTPS, or SSH as git@), 1 if not
  run -- HARNESS [ARGS...]       Start a session with user credentials; inside a
                                session, run HARNESS in it
  credential get                Git's credential helper, inside a session
  status                        Inside a session: each provider's credential, pending,
                                ready or failed with the reason
  sandbox check                 Inside a session: check that no protected path is
                                readable and that the session directory is
  ssh COMMAND                   Grant agents SSH certificates: grant, show, drop,
                                enroll, list-cas, name; see geas ssh --help

Requires Git, and glab in PATH for the glab issuer; geas ssh needs ssh-keygen,
and ykman and fido2-token for the YubiKey. Linux/macOS only.`)
}
func execute(ctx context.Context, args []string, api *api.API) (int, error) {
	if len(args) > 0 && args[0] == "_reaper" {
		return 0, session.RunReaper(ctx, args[1:])
	}
	// Git runs the helper inside a session, which has no configuration path.
	if len(args) > 0 && args[0] == "credential" {
		if len(args) != 2 {
			return 2, errors.New("usage: geas credential get")
		}
		return 0, credhelper.Run(args[1], os.Stdin, os.Stdout)
	}
	// Run inside a session, as `geas credential` is.
	if len(args) > 0 && args[0] == "status" {
		if len(args) != 1 {
			return 2, errors.New("usage: geas status")
		}
		return status(os.Stdout)
	}
	if len(args) > 0 && args[0] == "sandbox" {
		if len(args) != 2 || args[1] != "check" {
			return 2, errors.New("usage: geas sandbox check")
		}
		return sandboxCheck(os.Stdout)
	}
	// geas ssh reads no configuration: its CA lists are files of their own.
	if len(args) > 0 && args[0] == "ssh" {
		return sshgrant.New().Run(ctx, args[1:])
	}
	flags := flag.NewFlagSet("geas", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	path := flags.String("config", "", "configuration path (default $XDG_CONFIG_HOME/geas/config.json)")
	flags.Usage = func() { usage(os.Stderr) }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	rest := flags.Args()
	if len(rest) == 0 {
		usage(os.Stderr)
		return 2, errors.New("specify a command")
	}
	// Inside a session, `run` shares it: the configuration may be out of the
	// sandbox's reach, and the session already has its credentials.
	// Nested returns only when it cannot run the command.
	if rest[0] == "run" && os.Getenv("GEAS_SESSION_DIR") != "" {
		return session.ExitNotStarted, session.Nested(harness(rest[1:]))
	}
	timer := timing.New(os.Getenv("GEAS_DEBUG"), os.Stderr)
	if *path == "" {
		var err error
		if *path, err = config.DefaultPath(); err != nil && rest[0] == "run" {
			return session.ExitNotStarted, err
		} else if err != nil {
			return 1, err
		}
	}
	settings, err := config.Load(*path)
	timer.Step("config load")
	if err != nil && rest[0] == "run" {
		return session.ExitNotStarted, err
	}
	if err != nil {
		return 1, err
	}
	switch rest[0] {
	case "github-login":
		if len(settings.GitHub) == 0 {
			return 1, errors.New("no github provider in the configuration")
		}
		return 0, github.New(settings.GitHub[0], api).Login(ctx, os.Stdout)
	case "save-gitlab-issuer":
		return saveGitLabIssuer(ctx, settings, rest[1:])
	case "allowed":
		if len(rest) != 2 {
			return 2, errors.New("usage: geas allowed URL")
		}
		return allowed(os.Stdout, settings, rest[1])
	case "config":
		if len(rest) != 2 || rest[1] != "check" {
			return 2, errors.New("usage: geas config check")
		}
		configCheck(os.Stdout, *path, settings)
		return 0, nil
	case "run":
		return session.Run(ctx, settings, harness(rest[1:]), api, os.Stderr, timer)
	default:
		return 2, errors.New("unknown command")
	}
}

// saveGitLabIssuer stores the issuer PAT of the GitLab provider --host names,
// or of the only one whose issuer is "bearer".
func saveGitLabIssuer(ctx context.Context, settings config.Config, args []string) (int, error) {
	flags := flag.NewFlagSet("save-gitlab-issuer", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	host := flags.String("host", "", "")
	fromStdin := flags.Bool("stdin", false, "")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return 2, errors.New("usage: geas save-gitlab-issuer [--host HOST] [--stdin]")
	}
	var bearer []config.GitLabConfig
	for _, g := range settings.GitLab {
		if *host != "" && g.Host != *host {
			continue
		}
		if g.Issuer.Type == "bearer" {
			bearer = append(bearer, g)
		} else if *host != "" {
			return 1, errors.New(`the issuer of ` + g.Host + ` is "glab": your glab login, with no PAT to save`)
		}
	}
	switch {
	case len(bearer) == 0 && *host != "":
		return 1, errors.New("no gitlab provider for " + *host + " in the configuration")
	case len(bearer) == 0:
		return 1, errors.New(`no gitlab provider has a "bearer" issuer: a glab login has no PAT to save`)
	case len(bearer) > 1:
		return 1, errors.New("several gitlab providers have a bearer issuer: choose one with --host")
	}
	token, err := session.ReadIssuer(ctx, os.Stdin, os.Stderr, *fromStdin)
	if err != nil {
		return 1, err
	}
	if err = secretfile.Write(bearer[0].Issuer.TokenFile, []byte(token+"\n")); err != nil {
		return 1, errors.New("cannot save GitLab issuer")
	}
	fmt.Fprintln(os.Stdout, "Issuer for "+bearer[0].Host+" saved with mode 600.")
	return 0, nil
}

// allowed is geas allowed: it prints the provider whose credential a
// session's Git would get for the remote URL and exits 0, or exits 1 when
// none would. It reads only the configuration, with no network, and works
// outside a session as inside one.
func allowed(output io.Writer, settings config.Config, remote string) (int, error) {
	host, path, err := sessiondir.Remote(remote)
	if err != nil {
		return 1, err
	}
	hosts, err := session.GitHosts(settings)
	if err != nil {
		return 1, err
	}
	for _, h := range hosts {
		if h.Host == host && h.Allows(path) {
			fmt.Fprintln(output, h.Provider)
			return 0, nil
		}
	}
	return 1, nil
}

// configCheck reports a configuration that loaded, with its providers. An
// invalid one never gets here: Load's error names what is wrong, such as the
// first version 1 field.
func configCheck(output io.Writer, path string, settings config.Config) {
	fmt.Fprintf(output, "%s: version %d, session_hours %g\n", path, config.Version, settings.SessionHours)
	for _, g := range settings.GitLab {
		fmt.Fprintf(output, "  gitlab:%s  issuer %s, as %s\n", g.Host, g.Issuer.Type, g.ExpectedUser)
	}
	for _, g := range settings.GitHub {
		fmt.Fprintf(output, "  github:%s  as %s\n", g.Host, g.ExpectedUser)
	}
	if settings.SSH {
		fmt.Fprintln(output, "  ssh  the agent's certificate from geas ssh grant")
	}
}

// harness is the command after `run`, without its "--".
func harness(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		return args[1:]
	}
	return args
}

// sandboxCheck runs the sandbox check for the session GEAS_SESSION_DIR
// names, exiting 1 when a protected path is readable or the session
// directory is not.
func sandboxCheck(output io.Writer) (int, error) {
	dir, err := sessiondir.FromEnv()
	if err != nil {
		return 1, err
	}
	ok, err := sandbox.Check(dir, output)
	if err != nil || !ok {
		return 1, err
	}
	return 0, nil
}

// statusPending is geas status's exit code when no provider failed but some
// are still pending: EX_TEMPFAIL, try again later.
const statusPending = 75

// status prints each provider's marker for the session GEAS_SESSION_DIR
// names, one line each, and exits 0 when all are ready, 1 when one failed,
// and statusPending while one is still pending.
func status(output io.Writer) (int, error) {
	dir, err := sessiondir.FromEnv()
	if err != nil {
		return 1, err
	}
	markers, err := dir.Markers()
	if err != nil {
		return 1, err
	}
	ids := slices.Sorted(maps.Keys(markers))
	width := 0
	for _, id := range ids {
		width = max(width, len(id))
	}
	code := 0
	for _, id := range ids {
		m := markers[id]
		line := fmt.Sprintf("%-*s  %s", width, id, m.State)
		switch m.State {
		case sessiondir.Failed:
			line += ": " + m.Reason
			code = 1
		case sessiondir.Pending:
			if code == 0 {
				code = statusPending
			}
		}
		fmt.Fprintln(output, line)
	}
	if len(ids) == 0 {
		fmt.Fprintln(output, "no providers in this session")
	}
	return code, nil
}

// sessionContext ends on Ctrl-C, SIGTERM, or SIGHUP from a closed terminal or
// pane: each stops the harness, and the GitLab PAT is revoked before anything
// is printed. SIGPIPE is caught, not ignored (the harness would inherit that),
// so a write to a stdout or stderr that has gone away fails instead of killing
// the launcher.
func sessionContext() (context.Context, context.CancelFunc) {
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
}
func main() {
	ctx, cancel := sessionContext()
	defer cancel()
	code, err := execute(ctx, os.Args[1:], api.New())
	if err != nil {
		// API errors are sanitized at their source; avoid printing paths/config secrets.
		var path *os.PathError
		if errors.As(err, &path) {
			fmt.Fprintln(os.Stderr, "geas: local file or permission error")
		} else {
			fmt.Fprintln(os.Stderr, "geas:", err)
		}
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
