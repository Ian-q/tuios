package session

import (
	"encoding/json"
	"strings"
)

// The verbs of what a card touches: set-task-touches and
// list-task-overlaps. add-task takes touches too, and claim-task answers
// with the overlaps of the card it took (verb_task.go). The patterns and the
// rule that compares them are task_touches.go.

// touchesRepo is the repository a call that names touches means: repo as
// given, else the one the calling pane works in. Empty when neither says:
// the touches are then kept and compared with nothing.
func (d *Daemon) touchesRepo(repo, window string, touches []string) string {
	if repo != "" || len(touches) == 0 {
		return repo
	}
	return d.paneMainRoot(window)
}

// setTaskTouchesParams are what set-task-touches takes.
type setTaskTouchesParams struct {
	Session    string   `json:"session"`
	ID         string   `json:"id"`
	Touches    []string `json:"touches"`
	Repo       string   `json:"repo"`
	Clear      bool     `json:"clear"`
	HumanNonce string   `json:"human_nonce"`
}

// verbSetTaskTouches answers set-task-touches: the card's touches are
// replaced, or with clear taken off. The answer names the open cards
// another pane holds that the new touches overlap. It refuses nothing for
// an overlap.
func (d *Daemon) verbSetTaskTouches(cs *connState, params json.RawMessage) (any, *verbError) {
	var p setTaskTouchesParams
	if verr := decodeParams(params, &p); verr != nil {
		return nil, verr
	}
	sess, verr := d.sessionForTaskID(cs, p.Session, p.ID)
	if verr != nil {
		return nil, verr
	}
	touches, verr := cleanTouches("touches", p.Touches)
	if verr != nil {
		return nil, verr
	}
	if p.Clear && len(touches) > 0 {
		return nil, invalidParam("clear", "pass touches or clear, not both")
	}
	if !p.Clear && len(touches) == 0 {
		return nil, invalidParam("touches", "touches is required: the paths the card's work changes. Pass clear to take them off")
	}
	repo, verr := repoParam("repo", p.Repo)
	if verr != nil {
		return nil, verr
	}
	caller, verr := d.taskCallerOf(cs, "set-task-touches", sess, p.HumanNonce)
	if verr != nil {
		return nil, verr
	}
	// Read before the change: it reads the pane's directory.
	repo = d.touchesRepo(repo, caller.window, touches)
	reason := touchReasonCleared
	if len(touches) > 0 {
		reason = printableClaim(strings.Join(touches, " "), taskMaxReason)
	}
	row, action, spec, verr := d.changeTask(sess, p.ID, func(_ *SessionState, tk *TaskState, now int64) (string, *verbError) {
		if verr := caller.mayTouch("set-task-touches", tk); verr != nil {
			return "", verr
		}
		action, _, _ := sess.moveCard(tk, cardMove{kind: moveTouches, by: caller.by, reason: reason, touches: touches, repo: repo}, now)
		return action, nil
	}, func(action string) taskEventSpec {
		if action == "" {
			return taskEventSpec{}
		}
		return taskEventSpec{EventTaskChanged, action, "", reason}
	})
	if verr != nil {
		return nil, verr
	}
	if action != "" {
		d.afterTaskSpec(sess, spec, row)
	}
	row.Overlaps = d.heldOverlaps(cs, sess, row)
	return row.typed("task_changed"), nil
}

// listTaskOverlapsParams are what list-task-overlaps takes.
type listTaskOverlapsParams struct {
	Session     string   `json:"session"`
	ID          string   `json:"id"`
	Touches     []string `json:"touches"`
	Repo        string   `json:"repo"`
	AllSessions bool     `json:"all_sessions"`
}

// verbListTaskOverlaps answers list-task-overlaps: the open cards whose
// touches overlap those of the card id, or the touches given, in the same
// repository. It reads the session, or with all_sessions every session of
// the daemon.
func (d *Daemon) verbListTaskOverlaps(cs *connState, params json.RawMessage) (any, *verbError) {
	var p listTaskOverlapsParams
	if verr := decodeParams(params, &p); verr != nil {
		return nil, verr
	}
	touches, verr := cleanTouches("touches", p.Touches)
	if verr != nil {
		return nil, verr
	}
	if (p.ID != "") == (len(touches) > 0) {
		return nil, invalidParam("id", "name a card with id, or the paths with touches, not both")
	}
	repo, verr := repoParam("repo", p.Repo)
	if verr != nil {
		return nil, verr
	}
	sess, verr := d.resolveVerbSession(cs, p.Session)
	if verr != nil {
		return nil, verr
	}
	scope := overlapScope{sessions: []*Session{sess}}
	if p.AllSessions {
		scope.sessions = d.sessionsByName()
	}
	res := map[string]any{"type": "task_overlaps", "session": sess.Name(), "all_sessions": p.AllSessions}
	if p.ID != "" {
		if repo != "" {
			return nil, invalidParam("repo", "a card names its own repository, so repo goes with touches only")
		}
		tk, ok := sess.taskByID(p.ID)
		if !ok {
			return nil, noTask(sess, p.ID)
		}
		touches, repo = tk.Touches, tk.Repo
		scope.skipSession, scope.skipID = sess.Name(), tk.ID
		res["id"] = tk.ID
	} else if repo = d.touchesRepo(repo, d.callerWindow(cs), touches); repo == "" {
		return nil, hintedVerbError(ErrVerbInvalidParams, "no repository to read the touches in: the caller is in no pane inside one", &VerbHint{
			Param:   "repo",
			Command: "tuios task overlaps --repo <dir> --touches <pattern>",
			Detail:  "Nothing was read. Touches are compared inside one repository. Pass repo, the absolute path of its main checkout.",
		})
	}
	if touches == nil {
		touches = []string{}
	}
	res["repo"], res["touches"] = repo, touches
	res["overlaps"] = overlappingCards(scope, repo, touches)
	return res, nil
}
