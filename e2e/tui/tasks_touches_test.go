package tuie2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gaurav-Gosain/tuios/internal/testutil"
	"github.com/Gaurav-Gosain/tuitest"
)

// touchCard is tuios task get --json as the touches test reads it.
type touchCard struct {
	ID      string   `json:"id"`
	Stage   string   `json:"stage"`
	Touches []string `json:"touches"`
	Repo    string   `json:"repo"`
	Holder  *struct {
		Window string `json:"window"`
	} `json:"holder"`
	Overlaps []touchOverlap `json:"overlaps"`
}

// touchOverlap is one card of an overlaps answer.
type touchOverlap struct {
	Session string `json:"session"`
	ID      string `json:"id"`
	Stage   string `json:"stage"`
	Holder  *struct {
		Window string `json:"window"`
	} `json:"holder"`
	Patterns []struct {
		Yours  string `json:"yours"`
		Theirs string `json:"theirs"`
	} `json:"patterns"`
}

func getTouchCard(t *testing.T, base, session, id string) touchCard {
	t.Helper()
	var c touchCard
	out := mustCLI(t, base, "task", "get", "-s", session, id, "--json")
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		t.Fatalf("task get --json: %v\n%s", err, out)
	}
	return c
}

// TestTaskTouchesWarnAcrossWorktreesAndSurviveARestart: one repository, its
// main checkout and a linked worktree, and a session for each, as a person
// runs one session for each worktree. A card added from the main checkout
// names a folder, and a card added from the worktree names a file in it.
// tuios task overlaps finds the other card only with --all-sessions. A pane
// of the first session takes its card. A pane of the second then claims its
// own from inside the worktree: the claim is made, its standard output is the
// card alone, and its standard error names the first card, its session, its
// holder and the two patterns. After restart-server both cards still name
// their touches and their repository, and still overlap.
//
// How it could pass wrongly: the two cards could be compared as two
// repositories and the test still see a warning from some other source, so
// both cards' repo is read back and must be the main checkout; the claim
// could have been refused, so its exit status is read and the listing must
// show the pane as the holder; the warning could be on standard output, where
// a script reads the id, so the two streams go to two files.
//
// Negative controls: with the repository read as the checkout's own root in
// the place of the main one (worktree.MainRoot answering the worktree), the
// two cards read two repos and nothing overlaps. With claim-task not reading
// the held overlaps, the claim's standard error is empty. With the touches
// left out of the saved card, the cards after the restart name none.
func TestTaskTouchesWarnAcrossWorktreesAndSurviveARestart(t *testing.T) {
	base := t.TempDir()
	killDaemon(t, base)
	repo := testutil.GitRepo(t)
	linked := filepath.Join(filepath.Dir(repo), "linked")
	testutil.Git(t, repo, "worktree", "add", "-q", "-b", "feat/touches", linked)

	const sessA, sessB = "e2e-touch-main", "e2e-touch-linked"
	for _, s := range []string{sessA, sessB} {
		mustCLI(t, base, "new", s, "--detach")
		waitForSessionInfo(t, base, s)
	}
	var report strings.Builder
	run := func(dir string, args ...string) string {
		t.Helper()
		out, err := tuiosCLIInDir(t, base, dir, nil, args...)
		fmt.Fprintf(&report, "$ (in %s) tuios %s\n%s\n", filepath.Base(dir), strings.Join(args, " "), out)
		if err != nil {
			t.Fatalf("%v in %s: %v\n%s", args, dir, err, out)
		}
		return out
	}

	run(repo, "task", "add", "-s", sessA, "--touches", "internal/session/", "--touches", "docs/tasks.md", "rework the session store")
	run(linked, "task", "add", "-s", sessB, "--touches", "./internal/session/task.go", "fix the claim race")
	run(linked, "task", "add", "-s", sessB, "--touches", "cmd/**/*.go", "an unrelated card")

	a, b := getTouchCard(t, base, sessA, "t1"), getTouchCard(t, base, sessB, "t1")
	if a.Repo != repo || b.Repo != repo {
		t.Fatalf("ASSERTION: the card added in the main checkout reads repo %q and the one added in the linked worktree %q, want %q for both", a.Repo, b.Repo, repo)
	}
	if strings.Join(a.Touches, " ") != "internal/session/ docs/tasks.md" || strings.Join(b.Touches, " ") != "internal/session/task.go" {
		t.Fatalf("ASSERTION: the cards keep touches %q and %q", a.Touches, b.Touches)
	}

	// The other card is in another session: only --all-sessions reads it.
	if out := run(linked, "task", "overlaps", "-s", sessB, "t1"); !strings.Contains(out, "No open card") {
		t.Errorf("ASSERTION: overlaps within the session names a card:\n%s", out)
	}
	out := run(linked, "task", "overlaps", "-s", sessB, "t1", "--all-sessions")
	if !containsAll(out, sessA, "internal/session/task.go", "internal/session/", "rework the session store") || strings.Contains(out, "docs/tasks.md") {
		t.Errorf("ASSERTION: overlaps --all-sessions does not name the first session's card and the two patterns that meet, and only those:\n%s", out)
	}
	// Paths with no card, read in the repository of the folder.
	if out := run(linked, "task", "overlaps", "-s", sessB, "--touches", "cmd/tuios/main.go"); !containsAll(out, "t2", "cmd/**/*.go") || strings.Contains(out, "t1") {
		t.Errorf("ASSERTION: overlaps --touches cmd/tuios/main.go does not name t2 alone:\n%s", out)
	}

	holderA := strings.TrimSpace(mustCLI(t, base, "new-window", "-s", sessA, "main-worker", "--no-focus", "--print-id"))
	if out := run(repo, "task", "claim", "-s", sessA, "t1", "-w", holderA); strings.Contains(out, "Warning") {
		t.Fatalf("ASSERTION: the first claim, with no other card held, warns:\n%s", out)
	}

	// The second claim is made by a pane, from inside the worktree.
	paneB := strings.TrimSpace(mustCLI(t, base, "new-window", "-s", sessB, "linked-worker", "--no-focus", "--print-id"))
	stdout, stderr, done := filepath.Join(base, "claim.out"), filepath.Join(base, "claim.err"), filepath.Join(base, "claim.done")
	script := "cd " + linked + " && " + tuiosBin + " task claim t1 > " + stdout + " 2> " + stderr + "; echo EXIT=$? > " + done + ".tmp; mv " + done + ".tmp " + done + "\n"
	mustCLI(t, base, "send-text", "-s", sessB, "-w", paneB, script)
	status := waitForFileText(t, done, func(s string) bool { return strings.Contains(s, "EXIT=") }, "the pane never finished its claim")
	gotOut, gotErr := readText(t, stdout), readText(t, stderr)
	fmt.Fprintf(&report, "the pane's claim: %sstdout:\n%sstderr:\n%s\n", status, gotOut, gotErr)
	if !strings.Contains(status, "EXIT=0") {
		t.Fatalf("ASSERTION: the claim of a card that overlaps a held one failed, and it must only warn:\n%s%s", gotOut, gotErr)
	}
	if !strings.HasPrefix(gotOut, "t1 fix the claim race") || strings.Contains(gotOut, "Warning") {
		t.Errorf("ASSERTION: the claim's standard output is not the card alone:\n%s", gotOut)
	}
	if !containsAll(gotErr, "Warning", "overlaps t1", sessA, "main-worker", "internal/session/task.go with internal/session/", "Nothing is locked") {
		t.Fatalf("ASSERTION: the claim's standard error does not name the other card, its session, its holder and the patterns:\n%s", gotErr)
	}
	if got := getTouchCard(t, base, sessB, "t1"); got.Holder == nil || got.Holder.Window != paneB || got.Stage != "doing" {
		t.Fatalf("ASSERTION: after the claim the card reads %+v, want held by the pane %s in doing", got, paneB)
	}

	// The same answer as data: naming the touches again answers with the
	// held card in overlaps.
	raw := run(linked, "task", "touches", "-s", sessB, "t1", "internal/session/task.go", "internal/session/task_move.go", "--json")
	var set touchCard
	if err := json.Unmarshal([]byte(raw), &set); err != nil {
		t.Fatalf("task touches --json: %v\n%s", err, raw)
	}
	if len(set.Overlaps) != 1 || set.Overlaps[0].Session != sessA || set.Overlaps[0].ID != "t1" || set.Overlaps[0].Holder == nil ||
		set.Overlaps[0].Holder.Window != holderA || len(set.Overlaps[0].Patterns) != 2 {
		t.Fatalf("ASSERTION: task touches --json carries overlaps %+v, want the first session's t1, held by %s, with two pairs of patterns", set.Overlaps, holderA)
	}

	mustCLI(t, base, "restart-server", "--yes")
	mustCLI(t, base, "new", "e2e-touch-other", "--detach")
	waitForSessionInfo(t, base, sessA)
	waitForSessionInfo(t, base, sessB)
	a, b = getTouchCard(t, base, sessA, "t1"), getTouchCard(t, base, sessB, "t1")
	if a.Repo != repo || b.Repo != repo || strings.Join(a.Touches, " ") != "internal/session/ docs/tasks.md" ||
		strings.Join(b.Touches, " ") != "internal/session/task.go internal/session/task_move.go" {
		t.Fatalf("ASSERTION: after the restart the cards read %+v and %+v, want their touches in %s", a, b, repo)
	}
	out = run(linked, "task", "overlaps", "-s", sessB, "t1", "--all-sessions")
	if !containsAll(out, sessA, "internal/session/task_move.go") {
		t.Errorf("ASSERTION: after the restart the two cards no longer overlap:\n%s", out)
	}
	if got := run(linked, "task", "get", "-s", sessB, "t1"); !containsAll(got, "touches internal/session/task.go internal/session/task_move.go", repo) {
		t.Errorf("ASSERTION: task get does not show the touches and the repository:\n%s", got)
	}

	run(linked, "task", "touches", "-s", sessB, "t1", "--clear")
	if out := run(linked, "task", "overlaps", "-s", sessB, "t1", "--all-sessions"); !strings.Contains(out, "names no touches") {
		t.Errorf("ASSERTION: a card with its touches cleared still overlaps:\n%s", out)
	}
	saveText(t, "tasks-touches", report.String())
}

// TestCardPreviewShowsWhatACardTouches: the board's card preview shows the
// paths a card names, so the person sees what an agent is in before two
// cards meet.
//
// How it could pass wrongly: the title could hold the same words, so the
// card's title names no path and the patterns are read after the preview's
// own label.
//
// Artifact: the preview with the touches, as a frame.
//
// Negative control: with ckCardBody not drawing the card's touches, the line
// never shows.
func TestCardPreviewShowsWhatACardTouches(t *testing.T) {
	base := t.TempDir()
	killDaemon(t, base)
	repo := testutil.GitRepo(t)
	const sess = "e2e-touch-preview"
	mustCLI(t, base, "new", sess, "--detach")
	waitForSessionInfo(t, base, sess)
	if out, err := tuiosCLIInDir(t, base, repo, nil, "task", "add", "-s", sess, "--touches", "internal/session/", "--touches", "docs/tasks.md", "rework the store"); err != nil {
		t.Fatalf("task add: %v\n%s", err, out)
	}

	term := attachIn(t, base, sess, startOpts{cols: 200, rows: 55, shippedLooks: true})
	if err := term.SendKeys(tuitest.Ctrl('b'), "i"); err != nil {
		t.Fatalf("open the agents view: %v", err)
	}
	waitText(t, term, "the agents view", "Board")
	if err := term.SendKeys("b"); err != nil {
		t.Fatalf("press b: %v", err)
	}
	waitText(t, term, "the board's columns", "Todo  1")
	if err := term.SendKeys(" "); err != nil {
		t.Fatalf("press space: %v", err)
	}
	waitText(t, term, "the touches in the preview", "Card t1", "Touches internal/session/  docs/tasks.md")
	saveCockpitFrames(t, term, "card-preview-touches-200x55")
}

func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
