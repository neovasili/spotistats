// Command spotistats is the local operator CLI for the Spotistats pipeline.
//
// Commands that touch storage read their configuration from the environment; see
// internal/config for the full list. The two most useful knobs while developing:
//
//	SPOTISTATS_DDB_ENDPOINT=http://localhost:8000   run against DynamoDB Local
//	SPOTISTATS_TOKEN_FILE=./.dev/refresh_token.json store the refresh token in a file
//
// With both set, the whole pipeline runs with no AWS account and no AWS credentials.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
)

const progName = "spotistats"

type command struct {
	name    string
	summary string
	usage   string
	run     func(ctx context.Context, args []string) error
}

func commands() []command {
	return []command{
		{
			name:    "auth",
			summary: "Authorise with Spotify and inspect the stored refresh token",
			usage:   "auth <login|status> [flags]",
			run:     runAuth,
		},
		{
			name:    "poll",
			summary: "Run one capture pass: fetch recently played and ingest it",
			usage:   "poll [flags]",
			run:     runPoll,
		},
		{
			name:    "rollup",
			summary: "Reconcile aggregates, refresh leaderboards and render snapshots",
			usage:   "rollup [flags]",
			run:     runRollup,
		},
		{
			name:    "serve",
			summary: "Run the query API locally for the frontend dev server",
			usage:   "serve [flags]",
			run:     runServe,
		},
		{
			name:    "dev-seed",
			summary: "Write synthetic listening data to a local table (development only)",
			usage:   "dev-seed [flags]",
			run:     runDevSeed,
		},
		{
			name:    "init-table",
			summary: "Create the DynamoDB table locally (production uses CDK)",
			usage:   "init-table [flags]",
			run:     runInitTable,
		},
		{
			name:    "backfill",
			summary: "Import the GDPR extended streaming history export (one-off, local)",
			usage:   "backfill [flags]",
			run:     runBackfill,
		},
		{
			name:    "backfill-prune",
			summary: "Delete API-sourced plays superseded by an imported export window",
			usage:   "backfill-prune -from <ts> -to <ts>",
			run:     runBackfillPrune,
		},
		{
			name:    "enrich",
			summary: "Backfill artist names and genres for artists already recorded",
			usage:   "enrich [flags]",
			run:     runEnrich,
		},
		{
			name:    "doctor",
			summary: "Diagnose unresolved leaderboard names (IDs showing instead of names)",
			usage:   "doctor [flags]",
			run:     runDoctor,
		},
		{
			name:    "config",
			summary: "Print the resolved configuration (secrets redacted)",
			usage:   "config",
			run:     runConfig,
		},
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "%s: interrupted\n", progName)
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("no command given")
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(os.Stdout)
		return nil
	}

	for _, c := range commands() {
		if c.name == args[0] {
			return c.run(ctx, args[1:])
		}
	}
	usage(os.Stderr)
	return fmt.Errorf("unknown command %q", args[0])
}

func usage(w *os.File) {
	fmt.Fprintf(w, "%s - Spotistats operator CLI\n\nUsage:\n  %s <command> [flags]\n\nCommands:\n",
		progName, progName)
	cs := commands()
	sort.Slice(cs, func(i, j int) bool { return cs[i].name < cs[j].name })
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, c := range cs {
		fmt.Fprintf(tw, "  %s\t%s\n", c.name, c.summary)
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\nRun '%s <command> -h' for command flags.\n", progName)
}

// newFlagSet builds a flag set that prints a consistent header on -h.
func newFlagSet(name, usageLine string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s %s\n\nFlags:\n", progName, usageLine)
		fs.PrintDefaults()
	}
	return fs
}

// bullet formats an operator-facing progress line. Output on stdout is for humans; anything
// machine-readable goes through the structured logger on stderr.
func bullet(format string, a ...any) {
	fmt.Printf("  "+format+"\n", a...)
}

func heading(format string, a ...any) {
	fmt.Printf("\n"+strings.TrimSpace(format)+"\n", a...)
}
