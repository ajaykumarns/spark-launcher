package sparky

import (
	"context"
	"time"
)

// LiveCatalog uses the same ready-target selection as inference. The static
// CLI catalog's common budget cannot describe a route whose target changes.
// A metadata read never loads, unloads or reserves a model.
func (c *Coordinator) LiveCatalog() Object {
	catalog := modelCatalog(c.cfg())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, raw := range arr(catalog["data"]) {
		row := obj(raw)
		row["context_window"] = row["context_length"]
		row["max_output_tokens"] = row["output_tokens"]
		if str(row["id"]) != "garuda/spark-auto" {
			continue
		}
		for _, key := range []string{"context_length", "context_window", "output_tokens", "max_output_tokens"} {
			delete(row, key)
		}
		row["dynamic"], row["available"] = true, false
		if bifrostAuto(c.cfg()) {
			row["routing"], row["request_model"] = "bifrost", "spark-auto"
			c.mu.Lock()
			for key, value := range c.autoPool {
				row[key] = value
			}
			c.mu.Unlock()
			continue
		}
		if err := c.reconfigure.lock(ctx); err != nil {
			continue
		}
		name, err := c.resolveAuto(ctx)
		if err == nil {
			var sel *runtimeSelection
			sel, err = c.loadedSelection(name)
			if err == nil {
				window := num(sel.Setting["context_tokens"])
				row["context_length"], row["context_window"] = window, window
				if output, known := sel.Model["output_tokens"]; known {
					row["output_tokens"], row["max_output_tokens"] = output, output
				}
				row["resolved_model"] = "garuda/" + name
				row["runtime_generation"] = sel.Record["generation"]
				row["nodes"] = sel.Nodes
				row["inputs"] = value(sel.Model, "inputs", []any{"text"})
				row["reasoning"] = value(sel.Model, "reasoning", false)
				row["available"] = true
			}
		}
		c.reconfigure.unlock()
	}
	return catalog
}
