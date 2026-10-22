package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/nolith/geas/internal/api"
	"github.com/nolith/geas/internal/config"
	"github.com/nolith/geas/internal/credhelper"
	"github.com/nolith/geas/internal/provider/github"
	"github.com/nolith/geas/internal/sandbox"
	"github.com/nolith/geas/internal/secretfile"
	"github.com/nolith/geas/internal/session"
	"github.com/nolith/geas/internal/sessiondir"
	"github.com/nolith/geas/internal/timing"
)

func usage(output io.Writer) {
	fmt.Fprintln(output, `Usage: geas [--config config.json] COMMAND

Commands:
  github-login                  Authorize your GitHub App once via device flow
  save-gitlab-issuer [--stdin]   Store an issuer PAT with mode 600 (not with issuer "glab")
  run -- HARNESS [ARGS...]       Start a session with user credentials
  credential get                Git's credential helper, inside a session
  sandbox check                 Inside a session: check that no protected path is
                                readable and that the session directory is

Requires gh, glab and Git in PATH for enabled providers. Linux/macOS only.`)
}
func execute(ctx context.Context, args []string, api *api.API) (int, error) {
	if len(args) > 0 && args[0] == "_proxy" {
		if len(args) < 3 {
			return 1, errors.New("invalid proxy invocation")
		}
		return 1, session.ProxyCLI(args[1], args[2], args[3:])
	}
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
	if len(args) > 0 && args[0] == "sandbox" {
		if len(args) != 2 || args[1] != "check" {
			return 2, errors.New("usage: geas sandbox check")
		}
		return sandboxCheck(os.Stdout)
	}
	flags := flag.NewFlagSet("geas", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	path := flags.String("config", "config.json", "configuration path")
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
	timer := timing.New(os.Getenv("GEAS_DEBUG"), os.Stderr)
	settings, err := config.Load(*path)
	timer.Step("config load")
	if err != nil {
		return 1, err
	}
	switch rest[0] {
	case "github-login":
		if !config.Enabled(settings.GitHub.Enabled) {
			return 1, errors.New("GitHub is disabled")
		}
		return 0, github.New(settings.GitHub, api).Login(ctx, os.Stdout)
	case "save-gitlab-issuer":
		if !config.Enabled(settings.GitLab.Enabled) {
			return 1, errors.New("GitLab is disabled")
		}
		if settings.GitLab.Issuer == "glab" {
			return 1, errors.New(`GitLab issuer is "glab": your glab login, with no PAT to save`)
		}
		fromStdin := len(rest) == 2 && rest[1] == "--stdin"
		if len(rest) > 1 && !fromStdin {
			return 2, errors.New("only --stdin is supported for save-gitlab-issuer")
		}
		token, err := session.ReadIssuer(ctx, os.Stdin, os.Stderr, fromStdin)
		if err != nil {
			return 1, err
		}
		if err = secretfile.Write(settings.GitLab.IssuerTokenFile, []byte(token+"\n")); err != nil {
			return 1, errors.New("cannot save GitLab issuer")
		}
		fmt.Fprintln(os.Stdout, "Issuer saved with mode 600.")
		return 0, nil
	case "run":
		command := rest[1:]
		if len(command) > 0 && command[0] == "--" {
			command = command[1:]
		}
		return session.Run(ctx, settings, command, api, os.Stderr, timer)
	default:
		return 2, errors.New("unknown command")
	}
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
