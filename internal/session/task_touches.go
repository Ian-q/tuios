package session

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Gaurav-Gosain/tuios/internal/worktree"
)

// What a card touches: the paths its work is expected to change, so two
// agents in one repository hear that their cards meet before they both edit
// the same file. It is awareness and never a lock: nothing here refuses a
// claim.
//
// A card's Touches are patterns under its Repo. Repo is the main checkout of
// the repository, with symbolic links resolved (worktree.MainRoot), so a
// linked worktree and its main checkout name the same repository. Two cards
// are compared only when their Repo is the same string, and a card with no
// Repo meets nothing. The daemon never reads a path a pattern names.
//
// A pattern is a path from the repository's root, with / between its
// segments:
//
//   - A pattern that ends in / is a folder, and covers everything under it.
//   - Any other pattern covers the paths it matches and no more.
//   - * stands for any run of characters inside one segment, and a segment
//     that is ** for any number of segments, none included.
//   - Every other character stands for itself, ? and [ too, and case counts.
//   - A pattern is anchored at the root: *.go covers the Go files there, and
//     **/*.go the ones in every folder.
//
// Two patterns overlap when some path is covered by both. That is worked out
// from the two patterns alone, and exactly: no file is listed, and for
// literal paths, folders and globs alike no shared path is missed and none
// is made up. a/ and a/b.go overlap, a/*.go and a/*_test.go overlap, a/*.go
// and a/*.md do not.
//
// One more case counts, the one a forgotten / makes. When a pattern matches
// a path and the other pattern spells out, with no glob, a path under it,
// the first one names a folder: if the second path exists, it cannot be
// anything else. So internal/session and internal/session/task.go overlap,
// and so do * and cmd/main.go. Where it cannot be ruled out this way it is
// reported. A folder that a glob only might match is not counted, or
// **/*.go would overlap every path there is: internal/* and
// internal/*/task.go do not overlap. Write the / to mean a folder.
//
// The verbs are in verb_task_touches.go.

// Bounds on a card's touches.
const (
	taskMaxTouches     = 32
	taskMaxTouchLen    = 200
	taskMaxRepoLen     = 1024
	touchAnySegments   = "**"
	touchReasonCleared = "cleared"
)

// TaskActionTouches is the history and event action of a change to what a
// card touches.
const TaskActionTouches = "touches"

// TouchPair is one pattern of the card or the list asked about and one
// pattern of another card that overlaps it.
type TouchPair struct {
	Yours  string `json:"yours"`
	Theirs string `json:"theirs"`
}

// TaskOverlap is another card whose touches overlap, and the patterns that
// do.
type TaskOverlap struct {
	Session  string      `json:"session"`
	ID       string      `json:"id"`
	Title    string      `json:"title"`
	Stage    string      `json:"stage"`
	Holder   *TaskHolder `json:"holder"`
	Patterns []TouchPair `json:"patterns"`
}

// cleanTouches checks a touches parameter and puts each pattern in the form
// the card keeps: no ./ or empty segments, at most one trailing /, no
// pattern twice.
func cleanTouches(param string, in []string) ([]string, *verbError) {
	if len(in) > taskMaxTouches {
		return nil, invalidParam(param, "a card names at most "+strconv.Itoa(taskMaxTouches)+" touches. Name a folder, or use a glob")
	}
	var out []string
	for _, raw := range in {
		p, why := cleanTouch(raw)
		if why != "" {
			return nil, hintedVerbError(ErrVerbInvalidParams, "touches pattern "+echoName(raw)+" is refused: "+why, &VerbHint{
				Param:  param,
				Detail: "Nothing changed. A pattern is a path from the repository's root, such as internal/session/, docs/tasks.md or cmd/**/*.go.",
			})
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// cleanTouch is one pattern in the form the card keeps, or why it is
// refused.
func cleanTouch(raw string) (pattern, why string) {
	p := strings.TrimSpace(raw)
	switch {
	case p == "":
		return "", "it is empty"
	case len(p) > taskMaxTouchLen:
		return "", "it is longer than " + strconv.Itoa(taskMaxTouchLen) + " bytes"
	case printableClaim(p, taskMaxTouchLen) != p:
		return "", "it holds a control character"
	case strings.HasPrefix(p, "/") || filepath.IsAbs(p) || strings.HasPrefix(p, "~"):
		return "", "it is not relative to the repository's root"
	case strings.Contains(p, `\`):
		return "", "it holds a backslash: segments are separated by /"
	}
	dir := strings.HasSuffix(p, "/")
	var segs []string
	for seg := range strings.SplitSeq(p, "/") {
		switch seg {
		case "", ".":
		case "..":
			return "", "it leaves the repository with .."
		default:
			segs = append(segs, seg)
		}
	}
	if len(segs) == 0 {
		return "", "it names the root itself: write ** for the whole repository"
	}
	p = strings.Join(segs, "/")
	if dir {
		p += "/"
	}
	return p, ""
}

// repoParam checks a repo parameter: the absolute path of a repository's
// main checkout, which the daemon keeps as a name and never opens.
func repoParam(param, repo string) (string, *verbError) {
	if repo == "" {
		return "", nil
	}
	// A path of the caller's machine: a / start is absolute whatever
	// system the daemon runs on.
	abs := filepath.IsAbs(repo) || strings.HasPrefix(repo, "/")
	if len(repo) > taskMaxRepoLen || printableClaim(repo, taskMaxRepoLen) != repo || !abs {
		return "", invalidParam(param, "repo is the absolute path of the repository's main checkout, at most "+strconv.Itoa(taskMaxRepoLen)+" bytes")
	}
	return filepath.Clean(repo), nil
}

// touchOverlaps is every pair of a pattern of yours and a pattern of theirs
// that overlap, in the order of yours. Both lists are as cleanTouches leaves
// them.
func touchOverlaps(yours, theirs []string) []TouchPair {
	var out []TouchPair
	for _, a := range yours {
		for _, b := range theirs {
			if touchesMeet(a, b) {
				out = append(out, TouchPair{Yours: a, Theirs: b})
			}
		}
	}
	return out
}

// touchesMeet reports whether two patterns overlap: some path is covered by
// both, or one matches a folder the other spells out a path under.
func touchesMeet(a, b string) bool {
	as, adir := touchSegments(a)
	bs, bdir := touchSegments(b)
	return segmentListsMeet(touchCover(as, adir), touchCover(bs, bdir)) ||
		spelledUnder(as, adir, bs) || spelledUnder(bs, bdir, as)
}

// touchSegments is a pattern's segments, and whether it is a folder.
func touchSegments(p string) (segs []string, dir bool) {
	return strings.Split(strings.TrimSuffix(p, "/"), "/"), strings.HasSuffix(p, "/")
}

// touchCover is the segment pattern of everything a pattern covers: a
// folder's has the ** that stands for what is under it.
func touchCover(segs []string, dir bool) []string {
	if !dir {
		return segs
	}
	return append(slices.Clone(segs), touchAnySegments)
}

// segmentListsMeet reports whether some path matches both segment patterns.
func segmentListsMeet(a, b []string) bool {
	return globMeet(a, b, func(seg string) bool { return seg == touchAnySegments }, segmentsMeet)
}

// spelledUnder reports whether the pattern a, not a folder, matches a path
// that b spells out a path under: a start of b, shorter than b, with no
// glob in it.
func spelledUnder(a []string, adir bool, b []string) bool {
	if adir {
		return false
	}
	for k := 1; k < len(b); k++ {
		if strings.Contains(b[k-1], "*") {
			return false
		}
		if segmentListsMeet(a, b[:k]) {
			return true
		}
	}
	return false
}

// segmentsMeet reports whether some name matches both segments, each a
// literal with * for any run of characters.
func segmentsMeet(a, b string) bool {
	if !strings.Contains(a, "*") && !strings.Contains(b, "*") {
		return a == b
	}
	return globMeet([]byte(a), []byte(b),
		func(c byte) bool { return c == '*' }, func(x, y byte) bool { return x == y })
}

// globMeet reports whether one sequence matches both a and b, two patterns
// over the same items: an item wild reports true for stands for any run of
// items, none included, and any two other items match the same thing when
// meet says so. It fills the table of "a[i:] and b[j:] share a match" from
// the ends, a row at a time, so the cost is len(a) times len(b) calls of
// meet and it never backtracks.
func globMeet[T any](a, b []T, wild func(T) bool, meet func(x, y T) bool) bool {
	n, m := len(a), len(b)
	// below is the row of i+1 while row is filled for i.
	row, below := make([]bool, m+1), make([]bool, m+1)
	for i := n; i >= 0; i-- {
		for j := m; j >= 0; j-- {
			switch {
			case i == n && j == m:
				row[j] = true
			case i < n && wild(a[i]):
				// It stands for nothing, or takes one more item of b.
				row[j] = below[j] || j < m && row[j+1]
			case j < m && wild(b[j]):
				row[j] = row[j+1] || i < n && below[j]
			default:
				row[j] = i < n && j < m && meet(a[i], b[j]) && below[j+1]
			}
		}
		row, below = below, row
	}
	return below[0]
}

// overlapScope says which cards a search for overlaps reads.
type overlapScope struct {
	// sessions are the sessions read, in the order of the answer.
	sessions []*Session
	// held leaves out a card no pane holds.
	held bool
	// skipSession and skipID name the card asked about, and skipHolder the
	// pane it is held by: a pane's own cards are not news to it.
	skipSession, skipID, skipHolder string
}

// overlappingCards is every open card in scope whose touches overlap
// touches in repo. It reads a snapshot of each session and holds no lock, so
// a long list of patterns stalls no one.
func overlappingCards(scope overlapScope, repo string, touches []string) []TaskOverlap {
	out := []TaskOverlap{}
	if repo == "" || len(touches) == 0 {
		return out
	}
	for _, sess := range scope.sessions {
		st := sess.GetState()
		for i := range st.Tasks {
			tk := &st.Tasks[i]
			switch {
			case tk.Repo != repo || tk.Stage == TaskStageDone:
				continue
			case sess.Name() == scope.skipSession && tk.ID == scope.skipID:
				continue
			case scope.held && tk.Holder == "":
				continue
			case scope.skipHolder != "" && tk.Holder == scope.skipHolder:
				continue
			}
			pairs := touchOverlaps(touches, tk.Touches)
			if len(pairs) == 0 {
				continue
			}
			row := taskRow(sess, st, tk)
			out = append(out, TaskOverlap{Session: row.Session, ID: row.ID, Title: row.Title, Stage: row.Stage, Holder: row.Holder, Patterns: pairs})
		}
	}
	return out
}

// sessionsByName is every session of the daemon in name order.
func (d *Daemon) sessionsByName() []*Session {
	sessions := d.manager.AllSessions()
	slices.SortFunc(sessions, func(a, b *Session) int { return strings.Compare(a.Name(), b.Name()) })
	return sessions
}

// readsEverySession reports whether the caller on cs may be told of cards in
// sessions other than the one it called on. A pane without admin and a
// connection restricted to its own session may not: list-tasks with
// all_sessions is refused to both, and an overlap names a card's title and
// holder as that listing does.
func (d *Daemon) readsEverySession(cs *connState) bool {
	if cs != nil {
		if sc := cs.scope.Load(); sc != nil && sc.own {
			return false
		}
	}
	pa := d.paneAuthority(cs)
	return pa == nil || pa.grants.Has(GrantAdmin)
}

// heldOverlaps is what the caller on cs is told when it takes row, a card
// of sess, or names what row touches: the open cards another pane holds
// whose touches overlap row's, in every session of this daemon, or in sess
// alone for a caller that reads no other session.
func (d *Daemon) heldOverlaps(cs *connState, sess *Session, row TaskRow) []TaskOverlap {
	if row.Repo == "" || len(row.Touches) == 0 {
		return nil
	}
	scope := overlapScope{sessions: []*Session{sess}, held: true, skipSession: row.Session, skipID: row.ID}
	if d.readsEverySession(cs) {
		scope.sessions = d.sessionsByName()
	}
	if row.Holder != nil {
		scope.skipHolder = holderKey(row.Holder)
	}
	if out := overlappingCards(scope, row.Repo, row.Touches); len(out) > 0 {
		return out
	}
	return nil
}

// holderKey is a row's holder as a card keeps it: the window, or the
// remoteHolder key for a pane of another machine.
func holderKey(h *TaskHolder) string {
	if h.Host != "" {
		return remoteHolder(h.Host, h.Window)
	}
	return h.Window
}

// paneMainRoot is the repository the pane window works in, as a card's Repo:
// the main checkout above the directory of the pane's shell, as the kernel
// holds it, or above the folder the pane started in. It is never the pane's
// OSC 7 report, which any program in the pane can print. Empty for no
// window, a pane of another machine, and a pane outside every repository.
func (d *Daemon) paneMainRoot(window string) string {
	sess := d.sessionHoldingWindow(window)
	if sess == nil {
		return ""
	}
	w, ok := findWindowState(sess.GetState(), window)
	if !ok || w.Host != "" {
		return ""
	}
	cwd := sess.windowStartDir(w.ID)
	if pty := sess.GetPTY(w.PTYID); pty != nil {
		if live, ok := pty.ProcessCwd(); ok && live != "" {
			cwd = live
		}
	}
	root, _ := worktree.MainRoot(cwd)
	return root
}

// callerWindow is the pane the call on cs comes from, empty for a caller in
// no pane of this daemon.
func (d *Daemon) callerWindow(cs *connState) string {
	pa := d.paneAuthority(cs)
	if pa == nil || pa.plugin != "" || pa.hosted || pa.window == unplacedWindow {
		return ""
	}
	return pa.window
}
