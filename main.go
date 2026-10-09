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
	"time"
)

func usage(output io.Writer) {
	fmt.Fprintln(output, `Usage: agent-auth [--config config.json] COMMAND

Commands:
  github-login                  Authorize your GitHub App once via device flow
  save-gitlab-issuer [--stdin]   Store an issuer PAT with mode 600 (not with issuer "glab")
  run -- HARNESS [ARGS...]       Start a session with user credentials

Requires gh, glab and Git in PATH for enabled providers. Linux/macOS only.`)
}
func execute(ctx context.Context, args []string, api *API) (int, error) {
	if len(args) > 0 && args[0] == "_proxy" {
		if len(args) < 3 {
			return 1, errors.New("invalid proxy invocation")
		}
		return 1, proxyCLI(args[1], args[2], args[3:])
	}
	if len(args) > 0 && args[0] == "_reaper" {
		return 0, runReaper(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "_credential" {
		operation := "get"
		if len(args) > 1 {
			operation = args[1]
		}
		return 0, credentialWith(operation, os.Stdin, os.Stdout, rpc)
	}
	flags := flag.NewFlagSet("agent-auth", flag.ContinueOnError)
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
	config, err := loadConfig(*path)
	if err != nil {
		return 1, err
	}
	switch rest[0] {
	case "github-login":
		if !enabled(config.GitHub.Enabled) {
			return 1, errors.New("GitHub is disabled")
		}
		github := &GitHub{Config: config.GitHub, API: api, Now: time.Now}
		return 0, github.login(ctx, os.Stdout)
	case "save-gitlab-issuer":
		if !enabled(config.GitLab.Enabled) {
			return 1, errors.New("GitLab is disabled")
		}
		if config.GitLab.Issuer == "glab" {
			return 1, errors.New(`GitLab issuer is "glab": your glab login, with no PAT to save`)
		}
		fromStdin := len(rest) == 2 && rest[1] == "--stdin"
		if len(rest) > 1 && !fromStdin {
			return 2, errors.New("only --stdin is supported for save-gitlab-issuer")
		}
		token, err := readIssuer(ctx, os.Stdin, os.Stderr, fromStdin)
		if err != nil {
			return 1, err
		}
		if err = atomicWrite(config.GitLab.IssuerTokenFile, []byte(token+"\n")); err != nil {
			return 1, errors.New("cannot save GitLab issuer")
		}
		fmt.Fprintln(os.Stdout, "Issuer saved with mode 600.")
		return 0, nil
	case "run":
		command := rest[1:]
		if len(command) > 0 && command[0] == "--" {
			command = command[1:]
		}
		return runSession(ctx, config, command, api, os.Stderr)
	default:
		return 2, errors.New("unknown command")
	}
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
	code, err := execute(ctx, os.Args[1:], newAPI())
	if err != nil {
		// API errors are sanitized at their source; avoid printing paths/config secrets.
		var path *os.PathError
		if errors.As(err, &path) {
			fmt.Fprintln(os.Stderr, "agent-auth: local file or permission error")
		} else {
			fmt.Fprintln(os.Stderr, "agent-auth:", err)
		}
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
