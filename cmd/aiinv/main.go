// Command aiinv is the AI Inventory server and its administration CLI.
//
// One binary does everything a self-hosted installation needs: it serves the
// API, applies its own schema, and mints the enrollment tokens agents register
// with. That is deliberate. The product's claim is that an organisation can run
// this with a container and a connection string, and a server nobody can obtain
// a token for is not something anybody can actually use.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ebellefontaine/ai-inventory/internal/config"
	"github.com/ebellefontaine/ai-inventory/internal/db"
)

func main() {
	// SIGINT and SIGTERM cancel the context every command runs under, which is
	// what turns an orchestrator's stop signal into a graceful shutdown rather
	// than a killed process with connections still open. A second signal is not
	// caught: stop() restores the default handler, so an operator who has run
	// out of patience can still interrupt a drain that is going nowhere.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "aiinv: %v\n", err)
		os.Exit(1)
	}
}

// run is main with everything it touches passed in, so that the CLI can be
// tested without a process, an environment, or a terminal.
//
// stdout carries results — today that is exactly one thing, the plaintext of a
// freshly minted token — and stderr carries everything else: logs, usage, and
// the human explanation around that token. The split is what makes
// `TOKEN=$(aiinv token create ...)` capture a token and nothing else.
func run(ctx context.Context, args []string, lookup config.LookupFunc, stdout, stderr io.Writer) error {
	// No subcommand means serve. A container's default command is the common
	// case, and `docker run aiinv` should be a running server.
	command, rest := "serve", []string(nil)
	if len(args) > 0 {
		command, rest = args[0], args[1:]
	}

	switch command {
	case "serve":
		return withApp(ctx, "serve", rest, lookup, stderr, serveCommand)
	case "migrate":
		return migrateCommand(ctx, rest, lookup, stderr)
	case "token":
		return tokenCommand(ctx, rest, lookup, stdout, stderr)
	case "help", "-h", "-help", "--help":
		usage(stdout)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", command)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `aiinv — a single pane for every AI agent in a business.

Usage:
  aiinv [serve]                serve the API (the default when no command is given)
  aiinv migrate                apply database migrations and exit
  aiinv token create           mint an enrollment token

Flags:
  aiinv token create --team <slug> --env <development|staging|production> [--name <label>]

Environment:
  AIINV_DATABASE_URL           PostgreSQL connection string (required)
  AIINV_LISTEN_ADDR            address to serve on (default ":8080")
  AIINV_LOG_LEVEL              debug, info, warn or error (default "info")
`)
}

// withApp loads the configuration, builds a logger, and hands both to a command
// that needs a running installation. Every such command wants the same three
// failure modes reported the same way, and one place to do it is one place for
// a missing AIINV_DATABASE_URL to be explained.
func withApp(
	ctx context.Context,
	name string,
	args []string,
	lookup config.LookupFunc,
	stderr io.Writer,
	fn func(context.Context, config.Config, *slog.Logger) error,
) error {
	if err := parseFlags(name, args, stderr, nil); err != nil {
		return err
	}

	cfg, logger, err := configure(lookup, stderr)
	if err != nil {
		return err
	}
	return fn(ctx, cfg, logger)
}

// migrateCommand applies the schema and exits. serve does this too; this exists
// for the operator who wants the schema moved deliberately, at a moment they
// chose, rather than as a side effect of a rollout.
func migrateCommand(ctx context.Context, args []string, lookup config.LookupFunc, stderr io.Writer) error {
	if err := parseFlags("migrate", args, stderr, nil); err != nil {
		return err
	}

	cfg, logger, err := configure(lookup, stderr)
	if err != nil {
		return err
	}

	logger.InfoContext(ctx, "applying database migrations")
	if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	logger.InfoContext(ctx, "migrations applied")
	return nil
}

func tokenCommand(ctx context.Context, args []string, lookup config.LookupFunc, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return errors.New("token: expected a subcommand (create)")
	}

	switch args[0] {
	case "create":
		return tokenCreateCommand(ctx, args[1:], lookup, stdout, stderr)
	default:
		usage(stderr)
		return fmt.Errorf("token: unknown subcommand %q (want create)", args[0])
	}
}

// tokenCreateCommand mints one enrollment token and shows it once.
//
// The token's whole value is that it is a secret, so it is written to stdout and
// nowhere else: not to the log, not into an error message, not into the audit
// trail. Only its hash is stored, which means this print is the only copy that
// will ever exist and the warning beside it is not a formality.
func tokenCreateCommand(ctx context.Context, args []string, lookup config.LookupFunc, stdout, stderr io.Writer) error {
	var team, environment, name string
	err := parseFlags("token create", args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&team, "team", "", "team slug the token is scoped to; created if it does not exist")
		fs.StringVar(&environment, "env", "", "environment the token is scoped to: development, staging or production")
		fs.StringVar(&name, "name", "", `label for the token, for operators reading the token list (default "<team>-<env>")`)
	})
	if err != nil {
		return err
	}

	if strings.TrimSpace(team) == "" {
		return errors.New("token create: --team is required")
	}
	if strings.TrimSpace(environment) == "" {
		return errors.New("token create: --env is required")
	}
	if strings.TrimSpace(name) == "" {
		// A label rather than an identity: the scope is what the token means,
		// so the scope is a sensible thing to call it when nobody says
		// otherwise.
		name = strings.TrimSpace(team) + "-" + strings.TrimSpace(environment)
	}

	cfg, logger, err := configure(lookup, stderr)
	if err != nil {
		return err
	}

	app, err := newApp(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer app.close()

	minted, err := app.createToken(ctx, team, environment, name)
	if err != nil {
		return err
	}

	fmt.Fprintln(stdout, minted.Plaintext)
	fmt.Fprintf(stderr, `
Enrollment token %s minted for team %q in %s.
This is the only time the token is shown: only its hash is stored, and it
cannot be recovered. Mint another one if it is lost.

Agents registering with it inherit that team and environment. Nothing they
send about their own ownership is honoured.
`, minted.ID, strings.ToLower(strings.TrimSpace(team)), strings.ToLower(strings.TrimSpace(environment)))

	return nil
}

// parseFlags builds a flag set for one subcommand, lets the caller declare its
// flags, and parses args.
//
// flag.ErrHelp is returned by Parse when the operator asked for -h, and the
// usage has already been printed by then; reporting it as an error would exit
// non-zero for a question that was answered.
func parseFlags(name string, args []string, stderr io.Writer, declare func(*flag.FlagSet)) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	if declare != nil {
		declare(fs)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%s: unexpected argument %q", name, fs.Arg(0))
	}
	return nil
}

// configure loads the environment and builds the logger every command logs to.
//
// Logs go to stderr, in JSON. stderr because stdout is reserved for results,
// and JSON because the first thing anyone does with a self-hosted server's logs
// is ship them somewhere that wants to index fields rather than re-parse a
// sentence.
func configure(lookup config.LookupFunc, stderr io.Writer) (config.Config, *slog.Logger, error) {
	cfg, err := config.Load(lookup)
	if err != nil {
		return config.Config{}, nil, fmt.Errorf("load configuration: %w", err)
	}

	level, err := parseLogLevel(cfg.LogLevel)
	if err != nil {
		return config.Config{}, nil, err
	}

	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: level}))
	return cfg, logger, nil
}

// parseLogLevel refuses a level it does not recognise rather than defaulting to
// info. An operator who set AIINV_LOG_LEVEL=verbose wants more output, and
// silently giving them less is the one outcome that leaves them debugging the
// wrong thing.
func parseLogLevel(level string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("AIINV_LOG_LEVEL %q is not one of debug, info, warn, error", level)
	}
}
