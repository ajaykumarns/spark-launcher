package sparky

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
)

type profileStartTask struct {
	index int // index in the full plan, including its stop phase
	step  Object
	nodes []string // every device the start or a conditional stop can use
}

func (c *Coordinator) profileStartTasks(cfg Object, steps []any) ([]profileStartTask, error) {
	var tasks []profileStartTask
	for index, raw := range steps {
		step := copyObject(obj(raw))
		if str(step["action"]) == "stop" {
			continue
		}
		if str(step["action"]) != "start" || len(stringsOf(step["nodes"])) == 0 {
			return nil, fail("The profile contains an invalid start step.")
		}
		if str(step["service_kind"]) != "" && str(step["workload"]) != "spark-ocr" {
			_, recipe, _, _, err := selectPlacement(cfg, str(step["workload"]), stringsOf(step["nodes"])[0])
			if err != nil {
				return nil, err
			}
			if floors := obj(recipe["minimum_available_gib"]); len(floors) > 0 {
				step["minimum_available_gib"] = copyObject(floors)
			}
		}
		nodes := unionNodes(stringsOf(step["nodes"]), keys(obj(step["minimum_available_gib"])))
		nodes = unionNodes(nodes, c.reservationNodes(str(step["workload"])))
		for _, rawPeer := range arr(step["stop_if_low_memory"]) {
			peer := obj(rawPeer)
			nodes = unionNodes(nodes, stringsOf(peer["nodes"]))
			// unloadLocked reserves both the saved placement and the actual
			// placement. Include both when a kept peer runs on an override.
			_, _, saved, _, err := selectPlacement(cfg, str(peer["workload"]), "")
			if err != nil {
				return nil, err
			}
			nodes = unionNodes(nodes, saved)
			nodes = unionNodes(nodes, c.reservationNodes(str(peer["workload"])))
		}
		tasks = append(tasks, profileStartTask{index: index, step: step, nodes: nodes})
	}
	return tasks, nil
}

func checkProfileStartMemory(ctx context.Context, rt *Runtime, step Object) error {
	floors := obj(step["minimum_available_gib"])
	if len(floors) == 0 {
		return nil
	}
	// Read again after predecessors and conditional stops finish. A previous
	// model's steady-state use can invalidate the original plan's reading.
	state, err := rt.snapshot(ctx, true)
	if err != nil {
		return err
	}
	for _, node := range keys(floors) {
		available, minimum := profileAvailableGiB(state, node), num(floors[node])
		if available < minimum {
			return fail("%s has %.1f GiB available; %s needs %.1f GiB at startup.", node, available, str(step["workload"]), minimum)
		}
	}
	return nil
}

// scheduleProfileStarts preserves plan order on each device. A two-device
// start waits for both lanes and blocks later steps on either device. A lane
// can advance as soon as its own predecessor finishes, without a batch wait.
// On failure, do not schedule more work; join all starts already in progress
// before returning their completed effects or changing gateway routes.
func scheduleProfileStarts(ctx context.Context, tasks []profileStartTask, run func(context.Context, profileStartTask) ([]any, error)) ([]any, error) {
	deps := make([][]int, len(tasks))
	last := map[string]int{}
	for i, task := range tasks {
		if len(task.nodes) == 0 {
			return nil, fail("A profile start has no device reservation.")
		}
		for _, node := range task.nodes {
			if previous, ok := last[node]; ok {
				deps[i] = append(deps[i], previous)
			}
			last[node] = i
		}
	}
	type outcome struct {
		index     int
		completed []any
		err       error
	}
	results := make(chan outcome, len(tasks))
	started, finished := make([]bool, len(tasks)), make([]bool, len(tasks))
	effects := make([][]any, len(tasks))
	failures := make([]error, len(tasks))
	active, remaining := 0, len(tasks)
	var firstErr error
	receive := func(result outcome) {
		active--
		remaining--
		finished[result.index] = true
		effects[result.index] = result.completed
		if result.err != nil {
			failures[result.index] = fmt.Errorf("Step %d (%s): %w", tasks[result.index].index+1, profileStepMessage(tasks[result.index].step), result.err)
			if firstErr == nil {
				firstErr = failures[result.index]
			}
		}
	}
	drain := func() {
		for {
			select {
			case result := <-results:
				receive(result)
			default:
				return
			}
		}
	}
	for remaining > 0 {
		drain()
		for i, task := range tasks {
			drain()
			if firstErr == nil {
				firstErr = ctx.Err()
			}
			if firstErr != nil {
				break
			}
			ready := !started[i]
			for _, predecessor := range deps[i] {
				ready = ready && finished[predecessor]
			}
			if !ready {
				continue
			}
			started[i], active = true, active+1
			go func(index int, task profileStartTask) {
				completed, err := run(ctx, task)
				results <- outcome{index, completed, err}
			}(i, task)
		}
		if active == 0 {
			break
		}
		if firstErr != nil {
			receive(<-results)
		} else {
			select {
			case result := <-results:
				receive(result)
			case <-ctx.Done():
				firstErr = ctx.Err()
			}
		}
	}
	completed := []any{}
	for _, steps := range effects {
		completed = append(completed, steps...)
	}
	// Keep every failure from starts that were already running, in plan
	// order. The first failure still stops further scheduling immediately.
	problems := []error{}
	for _, err := range failures {
		if err != nil {
			problems = append(problems, err)
		}
	}
	if len(problems) == 0 {
		return completed, firstErr
	}
	joined := errors.Join(problems...)
	if err := ctx.Err(); err != nil && !errors.Is(joined, err) {
		joined = errors.Join(joined, err)
	}
	if len(problems) > 1 {
		joined = fmt.Errorf("%d profile starts failed: %w", len(problems), joined)
	}
	return completed, joined
}

func (c *Coordinator) applyProfileSteps(ctx context.Context, cfg Object, steps []any, checkConfig func() error) ([]any, bool, error) {
	completed := []any{}
	modelChanged := false
	// Finish planned stops first. Their routing changes and ComfyUI admission
	// pauses are shared controller operations and remain sequential.
	for index, raw := range steps {
		step := obj(raw)
		if str(step["action"]) != "stop" {
			continue
		}
		c.profilePhase(profileStepMessage(step), index+1, len(steps))
		if err := checkConfig(); err != nil {
			return completed, modelChanged, err
		}
		var err error
		if kind := str(step["service_kind"]); kind != "" {
			err = c.applyProfileService(ctx, c.runtime.frozenClone(cfg), kind, step)
		} else {
			modelChanged = true
			_, err = c.unloadLocked(ctx, cfg, str(step["workload"]))
		}
		if err != nil {
			return completed, modelChanged, err
		}
		completed = append(completed, step)
	}
	tasks, err := c.profileStartTasks(cfg, steps)
	if err != nil {
		return completed, modelChanged, err
	}
	for _, task := range tasks {
		modelChanged = modelChanged || str(task.step["service_kind"]) == ""
	}
	var stoppedMu, unloadMu sync.Mutex
	stopped := map[string]bool{}
	starts, err := scheduleProfileStarts(ctx, tasks, func(ctx context.Context, task profileStartTask) (effects []any, err error) {
		step, model := task.step, str(task.step["workload"])
		stoppedMu.Lock()
		localStopped := maps.Clone(stopped)
		stoppedMu.Unlock()
		if boolean(step["only_if_stopped_for_startup"]) && !localStopped[model] {
			return nil, nil
		}
		phase := func(message string) { c.profileStepPhase(message, task.index+1, len(steps)) }
		phase(profileStepMessage(step))
		defer func() { c.profileStepDone(task.index+1, err) }()
		if err = checkConfig(); err != nil {
			return effects, err
		}
		// Each start uses its own snapshot cache and immutable configuration.
		rt := c.runtime.frozenClone(cfg)
		if kind := str(step["service_kind"]); kind != "" {
			// Service startup can allocate CPU/GPU memory too. Hold the same
			// exclusive device admission used for model startup through both
			// the fresh memory check and readiness. Otherwise an image job can
			// acquire the device between the check and the container start.
			c.reconcileJobs(ctx)
			id, acquireErr := c.acquire(ctx, model, task.nodes, true, 0, "", "", true)
			if acquireErr != nil {
				return effects, acquireErr
			}
			defer c.release(id)
			if err = checkConfig(); err != nil {
				return effects, err
			}
			if err = checkProfileStartMemory(ctx, rt, step); err == nil {
				err = c.applyProfileService(ctx, rt, kind, step)
			}
		} else {
			err = stopProfilePeersForStartup(step, localStopped, func() (Object, error) {
				return rt.snapshot(ctx, true)
			}, func(peer Object) error {
				// Hold this through route removal AND shutdown. Another
				// conditional stop must not add this ready peer back to routes.
				unloadMu.Lock()
				defer unloadMu.Unlock()
				if err := checkConfig(); err != nil {
					return err
				}
				name := str(peer["workload"])
				phase(profileStepMessage(peer) + " to make room for " + model)
				if _, err := c.unloadLocked(ctx, cfg, name); err != nil {
					return err
				}
				stoppedMu.Lock()
				stopped[name] = true
				stoppedMu.Unlock()
				effects = append(effects, profileStep("stop", name, stringsOf(peer["nodes"])))
				return nil
			})
			if err == nil {
				// The native supervisor checks the recipe's memory floor after
				// loadLocked acquires its exclusive device lease. A check here
				// would reject transient memory use by a job we must first drain.
				phase(profileStepMessage(step) + "; waiting for readiness")
				_, err = c.loadLocked(ctx, cfg, model, "", str(step["on"]), c.generationCopy(), true)
			}
		}
		if err == nil {
			effects = append(effects, step)
		}
		return effects, err
	})
	return append(completed, starts...), modelChanged, err
}
