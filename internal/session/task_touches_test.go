package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Gaurav-Gosain/tuios/internal/testutil"
	"github.com/Gaurav-Gosain/tuios/internal/worktree"
)

// What a card touches (task_touches.go). The rule that compares two patterns
// is a pure function with no end-to-end surface of its own: a wrong answer
// there shows only as a warning that never comes, so it is pinned here, with
// the verbs' refusals and the saved form. The commands are proved end to end
// in e2e/tui/tasks_touches_test.go.
//
// The ways it could fail, written before the code:
//
//  1. A folder does not meet a file under it, in either order, or a path
//     with its / forgotten does not meet a path spelled out under it.
//  2. A name meets another that only starts with it (internal/app and
//     internal/application), because the compare is by bytes, not segments.
//  3. A * crosses a /, so a/*.go meets a/b/c.go.
//  4. A ** does not stand for no segment at all, so a/**/b misses a/b.
//  5. Two globs that share a path are missed because neither is a prefix of
//     the other (a/*_test.go and a/task*).
//  6. Two globs with nothing in common are reported (a/*.go and a/*.md),
//     or **/*.go meets every path because any path might be a folder,
//     which would make every warning noise.
//  7. ? or [ is read as a glob, so app/[id]/page.tsx meets app/i/page.tsx.
//  8. The answer depends on which list is yours.
//  9. Cards of two repositories meet, or cards with no repository do.
// 10. A linked worktree and its main checkout count as two repositories.
// 11. A pattern leaves the repository, or is absolute, and is kept.
// 12. A card saved before the fields existed does not load, or a card with
//     touches comes back from a restart without them.
// 13. A claim is refused, or says nothing, when the card overlaps a held one.

func TestTouchPatternsOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		// Literal paths and folders: the exact answers.
		{"internal/session/task.go", "internal/session/task.go", true},
		{"internal/session/task.go", "internal/session/task_move.go", false},
		{"internal/session/", "internal/session/task.go", true},
		// The forgotten /: a path spelled out under a match makes it a folder.
		{"internal/session", "internal/session/task.go", true},
		{"internal/session", "internal/session/*.go", true},
		{"internal/*", "internal/session/task.go", true},
		{"*", "cmd/main.go", true},
		{"internal/*", "internal/*/task.go", false}, // a glob only might name a folder
		{"docs/tasks.md", "**/*.go", false},
		{"internal/session/", "internal/session", true},
		{"internal/", "internal/session/task.go", true},
		{"internal/app", "internal/application", false},
		{"internal/app/", "internal/application/x.go", false},
		{"internal/session/", "internal/app/", false},
		{"docs/tasks.md", "docs/", true},
		{"README.md", "docs/README.md", false},
		{"Docs/", "docs/a.md", false},
		// The whole repository meets everything.
		{"**", "internal/session/task.go", true},
		{"**", "*.md", true},
		// * stays inside one segment.
		{"internal/*/task.go", "internal/session/task.go", true},
		{"internal/*.go", "internal/session/task.go", false},
		{"a/*.go", "a/b/c.go", false},
		{"a/*", "a/b/c.go", true}, // a/b is matched, and c.go is spelled out under it
		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", false},
		{"a/*.go", "a/*.md", false},
		{"a/*.go", "a/*_test.go", true},
		{"a/*_test.go", "a/task*", true},
		{"a/x*", "a/*y", true},
		{"a/x*z", "a/xy", false},
		{"a/*b*", "a/abc", true},
		{"a/**b", "a/zzb", true}, // ** inside a segment is two *
		// ** is any number of segments, none included.
		{"a/**/b.go", "a/b.go", true},
		{"a/**/b.go", "a/x/y/b.go", true},
		{"a/**/b.go", "a/x/c.go", false},
		{"**/*.go", "internal/session/task.go", true},
		{"**/*.go", "docs/tasks.md", false},
		{"**/*.go", "docs/", true}, // a Go file may sit under docs
		{"**/testdata/**", "internal/session/testdata/a.json", true},
		{"**/testdata/**", "internal/session/task.go", false},
		{"cmd/**/main.go", "cmd/*/sub/", true},
		{"a/**/x/**/y", "a/y/x", false},
		{"a/**/x/**/y", "a/x/y", true},
		// ? and [ stand for themselves.
		{"app/[id]/page.tsx", "app/i/page.tsx", false},
		{"app/[id]/page.tsx", "app/[id]/", true},
		{"a/?.go", "a/b.go", false},
	}
	for _, tc := range cases {
		if got := touchesMeet(tc.a, tc.b); got != tc.want {
			t.Errorf("touchesMeet(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := touchesMeet(tc.b, tc.a); got != tc.want {
			t.Errorf("touchesMeet(%q, %q) = %v, want %v: the answer depends on the order", tc.b, tc.a, got, tc.want)
		}
	}
}

// TestTouchOverlapAgreesWithMatching checks the rule against the thing it
// stands for. matches is a second, plain reading of "this pattern covers this
// path", written by recursion. For every pair of patterns built from a small
// alphabet, touchesMeet must say yes exactly when some path is covered by
// both, or one matches a start of the other that has no glob in it. Three
// names and six segments hold a witness for every pair these patterns can
// make: each has at most three segments that need one of the path's.
func TestTouchOverlapAgreesWithMatching(t *testing.T) {
	segs := []string{"a", "ab", "b", "*", "a*", "*b", "**"}
	var patterns []string
	for _, x := range segs {
		for _, y := range append([]string{""}, segs...) {
			for _, z := range append([]string{""}, segs...) {
				if y == "" && z != "" {
					continue
				}
				p := strings.TrimRight(x+"/"+y+"/"+z, "/")
				patterns = append(patterns, p, p+"/")
			}
		}
	}
	names := []string{"a", "ab", "b"}
	var paths [][]string
	var grow func(prefix []string, depth int)
	grow = func(prefix []string, depth int) {
		if len(prefix) > 0 {
			paths = append(paths, slices.Clone(prefix))
		}
		if depth == 0 {
			return
		}
		for _, n := range names {
			grow(append(prefix, n), depth-1)
		}
	}
	grow(nil, 6)
	// covered[p] has a bit for each path the pattern covers.
	covered := map[string][]uint64{}
	for _, p := range patterns {
		ps, dir := touchSegments(p)
		if dir {
			ps = append(slices.Clone(ps), "**")
		}
		bits := make([]uint64, len(paths)/64+1)
		for i, path := range paths {
			if matches(ps, path) {
				bits[i/64] |= 1 << (i % 64)
			}
		}
		covered[p] = bits
	}
	under := func(a, b string) bool {
		as, adir := touchSegments(a)
		bs, _ := touchSegments(b)
		for k := 1; k < len(bs) && !adir && !strings.Contains(bs[k-1], "*"); k++ {
			if matches(as, bs[:k]) {
				return true
			}
		}
		return false
	}
	pairs, met := 0, 0
	for _, a := range patterns {
		for _, b := range patterns {
			want := under(a, b) || under(b, a)
			for i, w := range covered[a] {
				if w&covered[b][i] != 0 {
					want = true
					break
				}
			}
			if got := touchesMeet(a, b); got != want {
				t.Fatalf("touchesMeet(%q, %q) = %v, and reading every path says %v", a, b, got, want)
			}
			pairs++
			if want {
				met++
			}
		}
	}
	if met == 0 || met == pairs {
		t.Fatalf("of %d pairs %d overlap, so the check proves nothing", pairs, met)
	}
}

// matches reports whether the segment pattern covers path, by recursion.
func matches(pattern, path []string) bool {
	if len(pattern) == 0 {
		return len(path) == 0
	}
	if pattern[0] == "**" {
		return matches(pattern[1:], path) || len(path) > 0 && matches(pattern, path[1:])
	}
	return len(path) > 0 && nameMatches(pattern[0], path[0]) && matches(pattern[1:], path[1:])
}

func nameMatches(pattern, name string) bool {
	if pattern == "" {
		return name == ""
	}
	if pattern[0] == '*' {
		return nameMatches(pattern[1:], name) || name != "" && nameMatches(pattern, name[1:])
	}
	return name != "" && pattern[0] == name[0] && nameMatches(pattern[1:], name[1:])
}

func TestCleanTouches(t *testing.T) {
	got, verr := cleanTouches("touches", []string{" ./internal//session/ ", "internal/session/", "docs/./tasks.md", "cmd/**/*.go", "a//b///"})
	if verr != nil {
		t.Fatalf("cleanTouches refused good patterns: %v", verr.Message)
	}
	if want := []string{"internal/session/", "docs/tasks.md", "cmd/**/*.go", "a/b/"}; !slices.Equal(got, want) {
		t.Errorf("cleanTouches = %q, want %q", got, want)
	}
	for _, bad := range []string{"", "  ", "/etc/passwd", "../other/x.go", "a/../../b", "~/src", `a\b`, ".", "./", "a\x1b[31m", strings.Repeat("a", taskMaxTouchLen+1)} {
		if got, verr := cleanTouches("touches", []string{bad}); verr == nil {
			t.Errorf("cleanTouches(%q) = %q, want a refusal", bad, got)
		}
	}
	if _, verr := cleanTouches("touches", make([]string, taskMaxTouches+1)); verr == nil {
		t.Errorf("cleanTouches took more than %d patterns", taskMaxTouches)
	}
}

// overlapIDs is the session/id of each card in a verb's overlaps.
func overlapIDs(t *testing.T, res map[string]any) []string {
	t.Helper()
	list, _ := res["overlaps"].([]any)
	out := []string{}
	for _, o := range list {
		m := o.(map[string]any)
		out = append(out, m["session"].(string)+"/"+m["id"].(string))
	}
	return out
}

// TestTaskTouchesVerbs: add-task and set-task-touches keep the patterns and
// the repository, list-task-overlaps reads them across sessions and only
// inside one repository, and a claim of a card that overlaps a held one
// succeeds and says so.
func TestTaskTouchesVerbs(t *testing.T) {
	d, sp := startTestDaemon(t)
	_, a, b := twoWindowSession(t, d, "work")
	otherSess := makeSessionWithWindow(t, d, "other")
	otherWin := otherSess.GetState().Windows[0].ID
	c := dialVerb(t, sp)
	const repo, repo2 = "/src/api", "/src/web"
	add := func(session, title, repo string, touches ...string) map[string]any {
		t.Helper()
		p := map[string]any{"session": session, "title": title}
		if len(touches) > 0 {
			p["touches"] = touches
		}
		if repo != "" {
			p["repo"] = repo
		}
		return result(t, callP(c, t, "add-task", p))
	}

	t1 := add("work", "retry loop", repo, "./api//retry.go", "api/retry_test.go")
	if got := t1["touches"]; !slices.Equal(anyStrings(got), []string{"api/retry.go", "api/retry_test.go"}) || t1["repo"] != repo {
		t.Fatalf("add-task kept touches %v in repo %v, want the two cleaned paths in %s", got, t1["repo"], repo)
	}
	add("work", "api folder", repo, "api/")                           // t2
	add("work", "docs only", repo, "docs/")                           // t3
	add("work", "same path, other repository", repo2, "api/retry.go") // t4
	add("work", "no touches", "")                                     // t5
	add("other", "tests everywhere", repo, "**/*_test.go")            // other/t1
	// A shell outside every pane names no repository unless it passes one:
	// the touches are kept, and meet nothing.
	t6 := add("work", "no repository", "", "api/retry.go")
	if _, has := t6["repo"]; has || len(anyStrings(t6["touches"])) != 1 {
		t.Fatalf("a card added with touches and no repository reads %v", t6)
	}

	mustRefuse(t, callP(c, t, "add-task", map[string]any{"session": "work", "title": "x", "touches": []string{"../x"}}), ErrVerbInvalidParams, "add-task with a pattern that leaves the repository")
	mustRefuse(t, callP(c, t, "add-task", map[string]any{"session": "work", "title": "x", "repo": repo}), ErrVerbInvalidParams, "add-task with repo and no touches")
	mustRefuse(t, callP(c, t, "add-task", map[string]any{"session": "work", "title": "x", "touches": []string{"a"}, "repo": "src/api"}), ErrVerbInvalidParams, "add-task with a relative repo")

	// One session, then every session.
	got := result(t, callP(c, t, "list-task-overlaps", map[string]any{"session": "work", "id": "t1"}))
	if ids := overlapIDs(t, got); !slices.Equal(ids, []string{"work/t2"}) {
		t.Errorf("overlaps of t1 in its session = %v, want work/t2 alone: t3 is another folder, t4 another repository, t5 and t6 name none", ids)
	}
	got = result(t, callP(c, t, "list-task-overlaps", map[string]any{"session": "work", "id": "t1", "all_sessions": true}))
	if ids := overlapIDs(t, got); !slices.Equal(ids, []string{"other/t1", "work/t2"}) {
		t.Errorf("overlaps of t1 in every session = %v, want other/t1 and work/t2", ids)
	}
	first := got["overlaps"].([]any)[0].(map[string]any)
	pair := first["patterns"].([]any)[0].(map[string]any)
	if pair["yours"] != "api/retry_test.go" || pair["theirs"] != "**/*_test.go" || first["holder"] != nil {
		t.Errorf("the overlap with other/t1 reads %v, want api/retry_test.go with **/*_test.go and no holder", first)
	}
	// Patterns with no card.
	got = result(t, callP(c, t, "list-task-overlaps", map[string]any{"session": "work", "touches": []string{"docs/tasks.md"}, "repo": repo}))
	if ids := overlapIDs(t, got); !slices.Equal(ids, []string{"work/t3"}) {
		t.Errorf("overlaps of docs/tasks.md = %v, want work/t3", ids)
	}
	for id, why := range map[string]string{"t5": "a card with no touches", "t6": "a card with no repository"} {
		got = result(t, callP(c, t, "list-task-overlaps", map[string]any{"session": "work", "id": id, "all_sessions": true}))
		if ids := overlapIDs(t, got); len(ids) != 0 {
			t.Errorf("%s overlaps %v, want nothing", why, ids)
		}
	}
	mustRefuse(t, callP(c, t, "list-task-overlaps", map[string]any{"session": "work"}), ErrVerbInvalidParams, "list-task-overlaps with no card and no touches")
	mustRefuse(t, callP(c, t, "list-task-overlaps", map[string]any{"session": "work", "id": "t1", "touches": []string{"a"}}), ErrVerbInvalidParams, "list-task-overlaps with a card and touches")
	mustRefuse(t, callP(c, t, "list-task-overlaps", map[string]any{"session": "work", "touches": []string{"a"}}), ErrVerbInvalidParams, "list-task-overlaps with touches and no repository, outside a pane")
	mustRefuse(t, callP(c, t, "list-task-overlaps", map[string]any{"session": "work", "id": "t9"}), ErrVerbNoTask, "list-task-overlaps of an unknown card")

	// A claim with nothing held says nothing.
	claimed := result(t, callP(c, t, "claim-task", map[string]any{"session": "work", "id": "t2", "window": a}))
	if _, has := claimed["overlaps"]; has {
		t.Errorf("a claim with no other card held carries overlaps: %v", claimed["overlaps"])
	}
	// A claim that overlaps a held card is made, and names it.
	claimed = result(t, callP(c, t, "claim-task", map[string]any{"session": "work", "id": "t1", "window": b}))
	if holderWindow(claimed) != b {
		t.Fatalf("ASSERTION: the claim of t1 was not made: %v", claimed)
	}
	if ids := overlapIDs(t, claimed); !slices.Equal(ids, []string{"work/t2"}) {
		t.Fatalf("ASSERTION: the claim of t1 names overlaps %v, want work/t2, the held one, and not other/t1, which no pane holds", ids)
	}
	o := claimed["overlaps"].([]any)[0].(map[string]any)
	if holderWindow(o) != a || o["stage"] != TaskStageDoing {
		t.Errorf("the overlap names holder %v in stage %v, want window %s in doing", o["holder"], o["stage"], a)
	}
	// A card in another session held by a third pane is named too.
	result(t, callP(c, t, "claim-task", map[string]any{"session": "other", "id": "t1", "window": otherWin}))
	result(t, callP(c, t, "release-task", map[string]any{"session": "work", "id": "t1"}))
	claimed = result(t, callP(c, t, "claim-task", map[string]any{"session": "work", "id": "t1", "window": b}))
	if ids := overlapIDs(t, claimed); !slices.Equal(ids, []string{"other/t1", "work/t2"}) {
		t.Errorf("the claim of t1 names overlaps %v, want other/t1 and work/t2", ids)
	}
	// A pane's own cards are not news to it, and a done card is not open.
	result(t, callP(c, t, "release-task", map[string]any{"session": "work", "id": "t1"}))
	result(t, callP(c, t, "finish-task", map[string]any{"session": "other", "id": "t1"}))
	claimed = result(t, callP(c, t, "claim-task", map[string]any{"session": "work", "id": "t1", "window": a}))
	if _, has := claimed["overlaps"]; has {
		t.Errorf("a pane that holds both cards is told they overlap: %v", claimed["overlaps"])
	}

	// set-task-touches replaces, answers with the held overlaps, notes the
	// history once, and clears.
	set := result(t, callP(c, t, "set-task-touches", map[string]any{"session": "work", "id": "t3", "touches": []string{"api/"}, "repo": repo}))
	if !slices.Equal(anyStrings(set["touches"]), []string{"api/"}) || !slices.Equal(overlapIDs(t, set), []string{"work/t1", "work/t2"}) {
		t.Errorf("set-task-touches answered touches %v and overlaps %v, want api/ and the two cards pane a holds", set["touches"], overlapIDs(t, set))
	}
	result(t, callP(c, t, "set-task-touches", map[string]any{"session": "work", "id": "t3", "touches": []string{"api/"}, "repo": repo}))
	detail := result(t, callP(c, t, "get-task", map[string]any{"session": "work", "id": "t3"}))
	noted := 0
	for _, h := range detail["history"].([]any) {
		if h.(map[string]any)["action"] == TaskActionTouches {
			noted++
		}
	}
	if noted != 1 {
		t.Errorf("t3's history has %d touches entries after one change and one repeat, want 1", noted)
	}
	cleared := result(t, callP(c, t, "set-task-touches", map[string]any{"session": "work", "id": "t3", "clear": true}))
	if _, has := cleared["touches"]; has || cleared["repo"] != nil {
		t.Errorf("a cleared card reads touches %v and repo %v, want neither", cleared["touches"], cleared["repo"])
	}
	mustRefuse(t, callP(c, t, "set-task-touches", map[string]any{"session": "work", "id": "t3"}), ErrVerbInvalidParams, "set-task-touches with neither touches nor clear")
	mustRefuse(t, callP(c, t, "set-task-touches", map[string]any{"session": "work", "id": "t3", "touches": []string{"a"}, "clear": true}), ErrVerbInvalidParams, "set-task-touches with touches and clear")

	// A pane without admin names touches only for the card it holds, and
	// is told of held cards in its own session only: it lists no other.
	add("other", "api, held in another session", repo, "api/") // other/t2
	result(t, callP(c, t, "claim-task", map[string]any{"session": "other", "id": "t2", "window": otherWin}))
	add("work", "one api file", repo, "api/client.go") // t7
	setStrict(d, "read", "write", "task")
	p := placeConnections(d)
	cb := p.paneConn(t, sp, b)
	wantForbidden(t, "pane b naming the touches of a's card", callP(cb, t, "set-task-touches", map[string]any{"id": "t1", "touches": []string{"x"}}))
	claimed = result(t, callP(cb, t, "claim-task", map[string]any{"id": "t7"}))
	if ids := overlapIDs(t, claimed); !slices.Equal(ids, []string{"work/t2"}) {
		t.Errorf("ASSERTION: a pane without admin claimed t7 and was told of %v, want work/t2 alone: other/t2 is in a session it may not list", ids)
	}
	mustRefuse(t, callP(cb, t, "list-task-overlaps", map[string]any{"id": "t7", "all_sessions": true}), ErrVerbForbidden, "list-task-overlaps all_sessions from a pane without admin")
	// The same claim from a shell outside every pane names both.
	result(t, callP(c, t, "release-task", map[string]any{"session": "work", "id": "t7"}))
	claimed = result(t, callP(c, t, "claim-task", map[string]any{"session": "work", "id": "t7", "window": b}))
	if ids := overlapIDs(t, claimed); !slices.Equal(ids, []string{"other/t2", "work/t2"}) {
		t.Errorf("a shell's claim of t7 names overlaps %v, want other/t2 and work/t2", ids)
	}
}

func anyStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.(string))
	}
	return out
}

// TestTaskTouchesReadThePanesRepository: a pane that names touches and no
// repository gets the one its shell is in, and a pane in a linked worktree
// gets the same one as a pane in the main checkout, so their cards meet.
func TestTaskTouchesReadThePanesRepository(t *testing.T) {
	repo := testutil.GitRepo(t)
	linked := filepath.Join(filepath.Dir(repo), "linked")
	if _, err := worktree.Add(repo, linked, "feat/x", ""); err != nil {
		t.Fatalf("worktree.Add: %v", err)
	}
	// The worktree is reached through a symbolic link, as a folder under a
	// linked home is.
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(linked, alias); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	d, sp := startTestDaemon(t)
	sess, err := d.manager.CreateSession("work", &SessionConfig{}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	inMain, err := sess.AddDaemonWindowWith(NewWindowOptions{Title: "main", Cwd: repo}, nil)
	if err != nil {
		t.Fatal(err)
	}
	inLinked, err := sess.AddDaemonWindowWith(NewWindowOptions{Title: "linked", Cwd: alias}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]string{"the main checkout": inMain.ID, "the linked worktree": inLinked.ID} {
		if got := d.paneMainRoot(id); got != repo {
			t.Fatalf("paneMainRoot of the pane in %s = %q, want %q", name, got, repo)
		}
	}
	setStrict(d, "read", "write", "task")
	p := placeConnections(d)
	cm, cl := p.paneConn(t, sp, inMain.ID), p.paneConn(t, sp, inLinked.ID)
	one := result(t, callP(cm, t, "add-task", map[string]any{"title": "from the main checkout", "touches": []string{"internal/session/"}}))
	two := result(t, callP(cl, t, "add-task", map[string]any{"title": "from the worktree", "touches": []string{"internal/session/task.go"}}))
	if one["repo"] != repo || two["repo"] != repo {
		t.Fatalf("the two cards read repo %v and %v, want %s for both", one["repo"], two["repo"], repo)
	}
	result(t, callP(cm, t, "claim-task", map[string]any{"id": "t1"}))
	claimed := result(t, callP(cl, t, "claim-task", map[string]any{"id": "t2"}))
	if ids := overlapIDs(t, claimed); !slices.Equal(ids, []string{"work/t1"}) {
		t.Errorf("ASSERTION: the pane in the worktree claimed t2 and was told of %v, want work/t1, held from the main checkout", ids)
	}
	got := result(t, callP(cl, t, "list-task-overlaps", map[string]any{"touches": []string{"internal/"}}))
	if ids := overlapIDs(t, got); !slices.Equal(ids, []string{"work/t1", "work/t2"}) || got["repo"] != repo {
		t.Errorf("list-task-overlaps from the worktree's pane read repo %v and %v, want both cards in %s", got["repo"], ids, repo)
	}
}

// TestTaskTouchesSurviveASave: a card's touches and repository are in the
// saved state and come back from it, and a state saved before the fields
// existed loads with cards that name none and overlap nothing.
func TestTaskTouchesSurviveASave(t *testing.T) {
	d, sp := startTestDaemon(t)
	_, a, _ := twoWindowSession(t, d, "work")
	c := dialVerb(t, sp)
	const repo = "/src/api"
	result(t, callP(c, t, "add-task", map[string]any{"session": "work", "title": "one", "touches": []string{"api/"}, "repo": repo}))
	result(t, callP(c, t, "add-task", map[string]any{"session": "work", "title": "two", "touches": []string{"api/retry.go"}, "repo": repo}))
	result(t, callP(c, t, "claim-task", map[string]any{"session": "work", "id": "t1", "window": a}))

	sess := d.manager.GetSession("work")
	if err := SaveSessionForResurrection(sess.ResurrectionState()); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadResurrectionState("work")
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Tasks) != 2 || !slices.Equal(saved.Tasks[0].Touches, []string{"api/"}) || saved.Tasks[1].Repo != repo {
		t.Fatalf("the saved cards read %+v, want both with their touches in %s", saved.Tasks, repo)
	}

	// The restore, into a session of another name on the same daemon.
	fresh := makeSessionWithWindow(t, d, "back")
	d.restoreTasks(fresh, saved.Tasks, saved.NextTaskID)
	got := result(t, callP(c, t, "list-task-overlaps", map[string]any{"session": "back", "id": "t2"}))
	if ids := overlapIDs(t, got); !slices.Equal(ids, []string{"back/t1"}) {
		t.Errorf("after the restore t2 overlaps %v, want back/t1", ids)
	}

	// A state from before the fields: the cards load as they were.
	old := []byte(`{"id":"t1","title":"old","stage":"doing","stage_at":5,"by":"shell","at":5,"history":[{"action":"added","by":"shell","at":5,"stage":"todo"}]}`)
	var tk TaskState
	if err := json.Unmarshal(old, &tk); err != nil {
		t.Fatal(err)
	}
	if tk.Touches != nil || tk.Repo != "" || tk.Title != "old" || len(tk.History) != 1 {
		t.Fatalf("a card saved before touches loads as %+v", tk)
	}
	again, err := json.Marshal(tk)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(again), "touches") || strings.Contains(string(again), "repo") {
		t.Errorf("a card with no touches is saved with the fields, so an older daemon's state grows: %s", again)
	}
	older := makeSessionWithWindow(t, d, "older")
	d.restoreTasks(older, []TaskState{tk}, 1)
	got = result(t, callP(c, t, "list-task-overlaps", map[string]any{"session": "older", "id": "t1", "all_sessions": true}))
	if ids := overlapIDs(t, got); len(ids) != 0 {
		t.Errorf("a card saved before touches overlaps %v, want nothing", ids)
	}
	row := result(t, callP(c, t, "get-task", map[string]any{"session": "older", "id": "t1"}))
	if _, has := row["touches"]; has {
		t.Errorf("a card saved before touches reads touches %v", row["touches"])
	}
}
