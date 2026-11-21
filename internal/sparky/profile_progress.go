package sparky

import (
	"fmt"
	"strings"
	"time"
)

// Progress is separate from the saved profile: the saved selection changes
// only after verification. All access is guarded by Coordinator.mu.
type profileProgress struct {
	id, name, status, message, problem string
	step, total                        int
	started, stepStarted, finished     time.Time
}

func (c *Coordinator) beginProfileProgress(name, id string) {
	if id == "" {
		id = token()
	}
	now := time.Now()
	c.mu.Lock()
	c.profileProgress = &profileProgress{id: id, name: name, status: "running",
		message: "Waiting for other model changes", started: now, stepStarted: now}
	c.mu.Unlock()
}

func (c *Coordinator) profilePhase(message string, step, total int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.profileProgress; p != nil {
		p.message, p.step, p.total, p.stepStarted = message, step, total, time.Now()
	}
}

func (c *Coordinator) finishProfileProgress(result Object, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.profileProgress
	if p == nil {
		return
	}
	p.finished = time.Now()
	p.status, p.problem = str(result["status"]), str(result["error"])
	if boolean(result["blocked"]) {
		p.status, p.problem = "blocked", strings.Join(stringsOf(result["blockers"]), " ")
	}
	if err != nil {
		p.status, p.problem = "failed", err.Error()
	}
	if p.status != "applied" && p.status != "blocked" {
		p.status = "failed"
	}
}

// ProfileProgress is a lightweight read. The request ID prevents one CLI
// from showing the progress of a different apply, including the same profile.
func (c *Coordinator) ProfileProgress(id string) Object {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.profileProgress
	if p == nil || (id != "" && id != p.id) {
		return Object{"status": "pending"}
	}
	end := p.finished
	if end.IsZero() {
		end = time.Now()
	}
	return Object{"id": p.id, "profile": p.name, "status": p.status,
		"message": p.message, "step": p.step, "total": p.total, "error": p.problem,
		"started_at": p.started.UTC(), "elapsed_seconds": max(0, int64(end.Sub(p.started)/time.Second)),
		"step_elapsed_seconds": max(0, int64(end.Sub(p.stepStarted)/time.Second))}
}

func profileStepMessage(step Object) string {
	return fmt.Sprintf("%s %s on %s", titleWord(str(step["action"])),
		str(step["workload"]), strings.Join(stringsOf(step["nodes"]), ", "))
}

func titleWord(word string) string {
	if word == "" {
		return word
	}
	return strings.ToUpper(word[:1]) + word[1:]
}
