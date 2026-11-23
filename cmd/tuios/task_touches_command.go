package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/Gaurav-Gosain/tuios/internal/errfmt"
	"github.com/Gaurav-Gosain/tuios/internal/jsonx"
	"github.com/Gaurav-Gosain/tuios/internal/session"
	"github.com/Gaurav-Gosain/tuios/internal/worktree"
	"github.com/spf13/cobra"
)

// What a task card touches: `tuios task touches`, `tuios task overlaps`, the
// --touches flag of `tuios task add`, and the warning a claim prints. The
// daemon's side is internal/session/task_touches.go.

// touchPatternHelp is what the commands say about a pattern.
const touchPatternHelp = `A pattern is a path from the root of the repository, with / between segments:

  internal/session/     a folder, and everything under it
  docs/tasks.md         one path
  cmd/*/main.go         * is any run of characters in one segment
  **/*_test.go          a segment ** is any number of segments, none included

Every other character stands for itself, and a pattern starts at the root:
*.go is the Go files there. End a folder with /. Quote a glob, or the shell
expands it.

Two cards overlap when one path is covered by a pattern of each, which tuios
works out from the patterns alone. Only cards of the same repository are
compared: the repository is the one the command runs in, and a linked git
worktree counts as its main checkout.`

// taskOverlap is one card that overlaps, as list-task-overlaps and a claim
// give it.
type taskOverlap struct {
	Session  string      `json:"session"`
	ID       string      `json:"id"`
	Title    string      `json:"title"`
	Stage    string      `json:"stage"`
	Holder   *taskHolder `json:"holder"`
	Patterns []struct {
		Yours  string `json:"yours"`
		Theirs string `json:"theirs"`
	} `json:"patterns"`
}

// addTouchFlags adds --touches and --repo.
func addTouchFlags(cmd *cobra.Command, touches *[]string, repo *string, usage string) {
	cmd.Flags().StringArrayVar(touches, "touches", nil, usage)
	cmd.Flags().StringVar(repo, "repo", "", "A folder of the repository the paths are in (default: the current folder)")
}

// touchParams puts touches and the repository they are read in into params.
// The repository is the main checkout above dir, or above the current folder,
// so a linked worktree names the same one as its main checkout. Outside every
// repository none is sent, and the daemon looks at the calling pane.
func touchParams(params map[string]any, touches []string, dir string) error {
	if len(touches) == 0 {
		if dir != "" {
			return errors.New("--repo says where --touches are read, so it goes with --touches only")
		}
		return nil
	}
	params["touches"] = touches
	named := dir != ""
	if !named {
		cwd, err := os.Getwd()
		if err != nil {
			return errfmt.Errorf("failed to read the current folder: %w", err)
		}
		dir = cwd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return errfmt.Errorf("failed to read %s: %w", dir, err)
	}
	root, ok := worktree.MainRoot(abs)
	switch {
	case ok:
		params["repo"] = root
	case named:
		return errfmt.Errorf("--repo %s is not inside a git repository", dir)
	}
	return nil
}

// staleTouches explains a daemon that refused touches or repo by name: it is
// older than this tuios, which a caller reads better than "has no parameter".
// It returns nil for every other error.
func staleTouches(err error) error {
	var call *session.VerbCallError
	if !errors.As(err, &call) || call.Code != session.ErrVerbInvalidParams || call.Hint == nil ||
		(call.Hint.Param != "touches" && call.Hint.Param != "repo") || !strings.Contains(call.Message, "has no parameter") {
		return nil
	}
	return &diagnosticError{
		What:  "The running daemon does not know what a card touches.",
		Cause: "the daemon is older than this tuios.",
		Fix:   "run 'tuios restart-server', or run the command again without --touches. Nothing changed.",
		Err:   err,
	}
}

// touchNotes are the lines a task command adds on standard error: a warning
// for each card another pane holds that overlaps row, and, for a command
// that named touches, why they will be compared with nothing.
func touchNotes(row taskRow, named bool) []string {
	var out []string
	switch {
	case !named:
	case len(row.Touches) == 0:
		// A daemon from before unknown parameters were refused drops them.
		out = append(out, "The running daemon is older than this tuios and did not keep the touches. Run 'tuios restart-server' to update it.")
	case row.Repo == "":
		out = append(out, "No git repository here, so these touches are compared with no other card. Run the command inside the repository, or pass --repo DIR.")
	}
	for _, o := range row.Overlaps {
		out = append(out, fmt.Sprintf("Warning: %s overlaps %s in session %s (%s, held by %s): %s. Nothing is locked. Agree with the holder who changes what.",
			plainLine(row.ID), plainLine(o.ID), plainLine(o.Session), plainLine(o.Stage), o.Holder.label(), o.pairs()))
	}
	return out
}

// pairs is the patterns that overlap, the first three spelled out.
func (o taskOverlap) pairs() string {
	var parts []string
	for i, p := range o.Patterns {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("and %d more", len(o.Patterns)-i))
			break
		}
		parts = append(parts, plainLine(p.Yours)+" with "+plainLine(p.Theirs))
	}
	return strings.Join(parts, ", ")
}

// newTaskTouchesCommand builds `tuios task touches`.
func newTaskTouchesCommand() *cobra.Command {
	var f taskFlags
	var clear bool
	var repo string
	cmd := &cobra.Command{
		Use:   "touches ID [PATTERN...] [--clear]",
		Short: "Say which paths a task card's work changes",
		Long: `Say which paths a task card's work changes, in the place of what it said
before. With no pattern it prints what the card says now, and --clear takes
them off. From inside a pane, only the card the pane holds.

Nothing is locked. 'tuios task overlaps' lists the open cards that meet, and
this command and 'tuios task claim' warn on standard error when the card meets
one another pane holds.

` + touchPatternHelp,
		Example: `  tuios task touches t1 internal/session/ docs/tasks.md
  tuios task touches t1 'cmd/**/*.go'
  tuios task touches t1
  tuios task touches t1 --clear`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			patterns := args[1:]
			switch {
			case clear && len(patterns) > 0:
				return errors.New("pass patterns or --clear, not both")
			case !clear && len(patterns) == 0:
				if repo != "" {
					return errors.New("--repo says where patterns are read, so it goes with patterns only")
				}
				return callTask(f, "", "get-task", map[string]any{"id": args[0]}, describeTouches)
			}
			params := map[string]any{"id": args[0]}
			if clear {
				params["clear"] = true
			}
			if err := touchParams(params, patterns, repo); err != nil {
				return err
			}
			return callTask(f, "", "set-task-touches", params, describeTouches)
		},
	}
	f.register(cmd)
	cmd.Flags().BoolVar(&clear, "clear", false, "Take the touches off the card")
	cmd.Flags().StringVar(&repo, "repo", "", "A folder of the repository the paths are in (default: the current folder)")
	return cmd
}

// describeTouches is what task touches prints: the card, then a pattern a
// line.
func describeTouches(r taskRow, on string) string {
	if len(r.Touches) == 0 {
		return fmt.Sprintf("%s%s names no touches.", plainLine(r.ID), on)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s touches", plainLine(r.ID), on)
	if r.Repo != "" {
		fmt.Fprintf(&b, ", in %s", plainLine(r.Repo))
	}
	b.WriteString(":")
	for _, p := range r.Touches {
		fmt.Fprintf(&b, "\n  %s", plainLine(p))
	}
	return b.String()
}

// newTaskOverlapsCommand builds `tuios task overlaps`.
func newTaskOverlapsCommand() *cobra.Command {
	var f taskFlags
	var touches []string
	var repo string
	var allSessions bool
	cmd := &cobra.Command{
		Use:   "overlaps [ID] [--touches PATTERN]... [--all-sessions]",
		Short: "List the open task cards whose touches overlap a card's, or some paths",
		Long: `List the open task cards (not done) whose touches overlap those of the card ID,
or the paths given with --touches: each card's session, stage and holder, and
the two patterns that meet. Use it before you take work, or before you add a
card, to see who else is in the same files.

It reads the session's cards, and with --all-sessions the cards of every
session on this machine, which is what finds an agent in another session of
the same repository.

` + touchPatternHelp,
		Example: `  tuios task overlaps t1 --all-sessions
  tuios task overlaps --touches internal/session/ --touches docs/tasks.md --all-sessions
  tuios task overlaps t1 --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if (len(args) == 1) == (len(touches) > 0) {
				return errors.New("name a card by its id, or the paths with --touches, not both")
			}
			params := map[string]any{}
			if len(args) == 1 {
				params["id"] = args[0]
			}
			if allSessions {
				params["all_sessions"] = true
			}
			if err := touchParams(params, touches, repo); err != nil {
				return err
			}
			return runTaskOverlaps(os.Stdout, f, params)
		},
	}
	f.register(cmd)
	addTouchFlags(cmd, &touches, &repo, "A path to compare, with no card; give it once per path")
	cmd.Flags().BoolVar(&allSessions, "all-sessions", false, "Read the cards of every session on this machine")
	return cmd
}

func runTaskOverlaps(w io.Writer, f taskFlags, params map[string]any) error {
	t, err := dialSessionTarget(f.session)
	if err != nil {
		return err
	}
	defer t.Close()
	raw, err := t.client.Call("list-task-overlaps", t.params(params))
	if err != nil {
		return reportVerbError(t.explain("list-task-overlaps", err), f.json)
	}
	if f.json {
		return printVerbResultOn(t, raw, true)
	}
	var res struct {
		Session     string        `json:"session"`
		AllSessions bool          `json:"all_sessions"`
		ID          string        `json:"id"`
		Repo        string        `json:"repo"`
		Touches     []string      `json:"touches"`
		Overlaps    []taskOverlap `json:"overlaps"`
	}
	if err := jsonx.Unmarshal(raw, &res); err != nil {
		return errfmt.Errorf("failed to parse response: %w", err)
	}
	what := "these paths"
	if res.ID != "" {
		what = plainLine(res.ID)
	}
	where := "session " + plainLine(res.Session) + t.on()
	if res.AllSessions {
		where = "any session" + t.on()
	}
	switch {
	case len(res.Touches) == 0:
		_, err = fmt.Fprintf(w, "%s names no touches, so nothing overlaps it. Name them with: tuios task touches %s PATTERN...\n", what, what)
		return err
	case res.Repo == "":
		_, err = fmt.Fprintf(w, "%s names touches in no repository, so nothing overlaps it. Name them again inside the repository: tuios task touches %s PATTERN...\n", what, what)
		return err
	case len(res.Overlaps) == 0:
		_, err = fmt.Fprintf(w, "No open card in %s overlaps %s.\n", where, what)
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tID\tSTAGE\tHOLDER\tYOURS\tTHEIRS\tTITLE")
	for _, o := range res.Overlaps {
		for _, p := range o.Patterns {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", plainLine(o.Session), plainLine(o.ID), plainLine(o.Stage),
				o.Holder.label(), plainLine(p.Yours), plainLine(p.Theirs), plainLine(o.Title))
		}
	}
	return tw.Flush()
}
