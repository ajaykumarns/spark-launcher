package sparky

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

var cliProfilePollInterval = 2 * time.Second

// Keep stdout as one final result. Progress uses stderr and a separate,
// bounded read so a failed progress probe never cancels a model change.
func cliApplyProfile(ctx context.Context, cfg clientConfig, payload Object, progress bool, out io.Writer) (Object, error) {
	if !progress {
		return cliRemoteRPC(ctx, cfg, "profile-apply", payload)
	}
	payload = copyObject(payload)
	id := token()
	payload["progress_id"] = id
	started := time.Now()
	fmt.Fprintf(out, "Applying profile %s. Elapsed: 0s.\n", str(payload["name"]))
	pollCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(cliProfilePollInterval)
		defer ticker.Stop()
		lastMessage := "Waiting for the controller"
		lastPrinted := started
		unavailable := false
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-ticker.C:
			}
			probeCtx, stop := context.WithTimeout(pollCtx, 5*time.Second)
			state, err := cliRemoteRPC(probeCtx, cfg, "profile-progress", Object{"id": id})
			stop()
			if pollCtx.Err() != nil {
				return
			}
			message := lastMessage
			stepTime := ""
			var parallelLines []string
			if err != nil {
				message = "Current step unavailable; still waiting for the apply result"
				unavailable = true
			} else if str(state["id"]) == id {
				message = strings.Join(strings.Fields(str(state["message"])), " ")
				if step := num(state["step"]); step > 0 {
					message = fmt.Sprintf("Step %.0f/%.0f: %s", step, num(state["total"]), message)
				}
				if stepSeconds := num(state["step_elapsed_seconds"]); stepSeconds >= 1 {
					stepTime = " (step: " + (time.Duration(stepSeconds) * time.Second).String() + ")"
				}
				if active := arr(state["active_steps"]); len(active) > 1 {
					labels := []string{}
					for _, raw := range active {
						step := obj(raw)
						label := fmt.Sprintf("Step %.0f/%.0f: %s", num(step["step"]), num(step["total"]), strings.Join(strings.Fields(str(step["message"])), " "))
						labels = append(labels, label)
						parallelLines = append(parallelLines, label+" (step: "+(time.Duration(num(step["elapsed_seconds"]))*time.Second).String()+")")
					}
					message = strings.Join(labels, " | ")
				}
				unavailable = false
			} else if unavailable {
				message = "Waiting for the controller to report this request"
			}
			if message != lastMessage || time.Since(lastPrinted) >= cliProgressInterval {
				if len(parallelLines) == 0 {
					parallelLines = []string{message + stepTime}
				}
				for _, line := range parallelLines {
					fmt.Fprintf(out, "[%s elapsed] %s\n", time.Since(started).Truncate(time.Second), line)
				}
				lastMessage, lastPrinted = message, time.Now()
			}
		}
	}()
	defer func() {
		cancel()
		<-done
		fmt.Fprintf(out, "Elapsed: %s.\n", time.Since(started).Truncate(time.Second))
	}()
	return cliRemoteRPC(ctx, cfg, "profile-apply", payload)
}
