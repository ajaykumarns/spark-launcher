//go:build darwin && cgo

#ifndef SPARKY_PANEL_H
#define SPARKY_PANEL_H

// Minimal string/flag bridge between Go (business logic) and AppKit (view).
// Go never runs AppKit calls off the main thread; every sparky_* function may
// be called from any goroutine and dispatches to the main queue internally.
// Text is copied by AppKit before returning, so Go string memory is not kept.

// sparky_init synchronously creates NSApplication (accessory policy) and the
// status item. It MUST be called on the process main OS thread, which
// main_darwin.go locks with runtime.LockOSThread before main.
void sparky_init(void);
// sparky_run runs the event loop of that main-thread-initialized NSApplication.
// It MUST be called on the same locked main OS thread, after sparky_init.
void sparky_run(void);
void sparky_quit(void);

// Panel text content. Pass NULL/"“" for optional lines to hide them.
void sparky_set_panel(const char *headerTitle, const char *headerState,
                      const char *modelCaption, const char *modelName,
                      const char *modelLocation,
                      const char *memoryCaption,
                      const char *refreshLabel,
                      const char *loadLabel,
                      const char *footer,
                      const char *loadResult);

// Extra loaded LLM lines (0..n), and device memory rows.
void sparky_set_extra_models(const char **lines, int count);
void sparky_set_memory_rows(const char **labels, const char **values, int count);
void sparky_set_requests(const char *label, const char *value);

// Picker content. picking is Go's authoritative mode. Choice buttons are only
// rebuilt when labels/IDs change, preserving keyboard focus across renders.
void sparky_set_picker(int picking, const char *pickerCaption,
                       const char **labels, const char **ids, int count);

// Interactive enablement and settings state.
void sparky_set_enabled(int loadEnabled, int refreshEnabled, int hasCLI);
void sparky_set_settings(int showTitle, int loginEnabled);

// Show or hide the popover (presentation only; never cancels work). Visibility
// is not queryable across threads: AppKit state is only read on the main thread.
void sparky_show_panel(void);
void sparky_hide_panel(void);

// Icon state: 0 normal, 1 loading, 2 error. Optional title text.
void sparky_set_icon_state(int state, const char *title);

// Headless construction probe support (used only by native tests). Builds the
// panel controller/popover in memory with no status item, window, or GUI loop.
// Call on the main thread; never dispatches to an unpumped main queue.
int sparky_probe_build_panel(void);
int sparky_probe_choice_count(void);
char *sparky_probe_picker_caption(void);
char *sparky_probe_layout(int picking);

#endif
