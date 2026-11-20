//go:build darwin && cgo

package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"sparky/internal/menubarui"
)

const (
	catalogBound = 30 * time.Second
	catalogFresh = 20 * time.Second
	pbcopyBound  = 5 * time.Second
	uiTick       = time.Second
	loadTick     = time.Second
)

// panelStrings mirrors the pre-formatted strings pushed to the AppKit bridge.
type panelStrings struct {
	HeaderTitle   string
	HeaderState   string
	ModelCaption  string
	ModelName     string
	ModelLocation string
	MemoryCaption string
	RefreshLabel  string
	LoadLabel     string
	Footer        string
	LoadResult    string
}

type panelPair struct {
	Label string
	Value string
}

// menuApp owns the panel. All subprocess work runs on background goroutines;
// AppKit calls are marshalled to the main thread by the bridge. uiMu serializes
// presentation updates, preference changes, picker mode, load acceptance and
// shutdown so the UI never races and never mutates after quit. Go is the single
// owner of picker state: AppKit only reports open/close/choice intent.
type menuApp struct {
	version string
	cli     string
	cliErr  error
	lock    *menubarui.Lock

	ctrl   menubarui.ProfileController
	poller *menubarui.Poller
	load   menubarui.LoadTracker

	uiMu          sync.Mutex
	done          bool // set when presentation must stop changing
	prefs         menubarui.Prefs
	profiles      []menubarui.Choice
	profileResult string
	catalogBusy   bool
	catalogAt     time.Time
	catalogErr    string // short friendly text of the last failed catalog fetch
	picking       bool

	forceRender chan struct{}
	stopUI      chan struct{}
	stopOnce    sync.Once
}

func runApp(app *menuApp) {
	if app.forceRender == nil {
		app.forceRender = make(chan struct{}, 1)
	}
	if app.stopUI == nil {
		app.stopUI = make(chan struct{})
	}
	activeApp = app
	// Both calls run on the locked main OS thread: panelInit synchronously
	// creates NSApplication and the status item, then panelRun starts that
	// app's event loop. No AppKit work is queued from another thread here.
	panelInit()
	app.onReady()
	panelRun()
}

// onReady starts background work, then renders cached data immediately.
func (a *menuApp) onReady() {
	go a.watchShutdown()
	go a.loopRender()
	go a.loopLoadStatus()

	a.render()
	if a.ctrl == nil {
		return
	}
	a.poller.Start()
	a.refreshAsync()
	a.loadProfilesAsync(false)
}

// watchShutdown lets the UI loops stop promptly when quit is clicked. It never
// releases the single-instance lock: quit can wait a long time for an accepted
// load, and releasing here would let a second instance start while this process
// still owns an active CLI child. The lock is held for the whole process
// lifetime and released only at final termination (or by the kernel on exit).
func (a *menuApp) watchShutdown() {
	<-a.stopUI
	if a.poller != nil {
		a.poller.Stop()
	}
}

func (a *menuApp) uiAlive() bool {
	a.uiMu.Lock()
	defer a.uiMu.Unlock()
	return !a.done
}

// render builds the presentation from current state and pushes it to AppKit.
func (a *menuApp) render() {
	a.uiMu.Lock()
	defer a.uiMu.Unlock()
	if a.done {
		return
	}
	a.renderLocked()
}

// renderLocked must be called with uiMu held.
func (a *menuApp) renderLocked() {
	var snap menubarui.Snapshot
	if a.ctrl != nil {
		snap = a.ctrl.Snapshot()
	} else {
		snap.Error = errText(a.cliErr)
	}
	// Merge accepted-load state immediately, before the backend goroutine has
	// updated Snapshot.Loading. This closes the acceptance gap that would
	// otherwise briefly re-enable Load and show no progress.
	profile, running := a.load.Running()
	snap = menubarui.MergeAcceptedLoad(snap, running)
	if running {
		// The tracker is the authoritative local timing for an accepted load.
		// Always show its elapsed time, together with a concise message naming
		// the target model; never invent stages or percentages.
		snap.LoadMessage = fmt.Sprintf("Profile %s · %s", profile, a.load.Elapsed().Round(time.Second))
	} else if snap.Loading && snap.LoadMessage == "" {
		snap.LoadMessage = "Loading a model…"
	}

	if a.catalogErr == "" {
		for _, choice := range a.profiles {
			if choice.ID == snap.ProfileName {
				snap.ProfileDescription = choice.Description
				break
			}
		}
	}
	v := menubarui.BuildPanel(snap, a.prefs, nil, a.picking, a.ctrl != nil)
	if running {
		v.HeaderState = "Switching profile"
	} else {
		v.LoadResult = a.profileResult
	}
	v.PickerCaption = a.pickerCaption()
	initialProfilesPending := a.ctrl != nil && a.profiles == nil && a.catalogErr == ""
	if initialProfilesPending {
		v.LoadRowLabel = "Loading profiles…"
		v.LoadEnabled = false
	}
	descriptions := make([]string, 0, len(a.profiles))
	for _, choice := range a.profiles {
		v.Choices = append(v.Choices, choice.Name)
		v.ChoiceIDs = append(v.ChoiceIDs, choice.ID)
		descriptions = append(descriptions, choice.Description)
	}

	panelSetContent(panelStrings{
		HeaderTitle:   v.HeaderTitle,
		HeaderState:   v.HeaderState,
		ModelCaption:  v.ModelCaption,
		ModelName:     v.ModelName,
		ModelLocation: v.ModelLocation,
		MemoryCaption: v.MemoryCaption,
		RefreshLabel:  v.RefreshRowLabel,
		LoadLabel:     v.LoadRowLabel,
		Footer:        v.Footer,
		LoadResult:    v.LoadResult,
	})
	panelSetExtraModels(v.ExtraModels)
	pairs := make([]panelPair, len(v.MemoryRows))
	for i, p := range v.MemoryRows {
		pairs[i] = panelPair{Label: p.Label, Value: p.Value}
	}
	panelSetMemoryRows(pairs)
	panelSetRequests(v.RequestsRow.Label, v.RequestsRow.Value)
	panelSetPicker(initialProfilesPending, v.PickerCaption, v.Choices, v.ChoiceIDs, descriptions, snap.ProfileName)
	panelSetEnabled(v.LoadEnabled, v.RefreshEnabled, v.HasCLI)
	panelSetSettings(v.ShowTitle, menubarui.LoginEnabled())

	iconState := 0
	if snap.Loading {
		iconState = 1
	} else if snap.Error != "" || a.ctrl == nil {
		iconState = 2
	}
	panelSetIcon(iconState, menubarui.IconTitle(snap, a.prefs.ShowTitle))
}

// pickerCaption is the short status line above the choices: loading, empty,
// error, or default instructions. Must be called with uiMu held.
func (a *menuApp) pickerCaption() string {
	switch {
	case a.catalogErr != "" && len(a.profiles) == 0:
		return "Profile list unavailable: " + a.catalogErr
	case a.catalogBusy && a.profiles == nil:
		return "Loading profiles…"
	case a.profiles == nil:
		return "Waiting for the profile list"
	case len(a.profiles) == 0:
		return "No valid profiles. Run sparky profile list."
	default:
		return "Select a profile"
	}
}

// loopRender redraws on a cheap tick and on explicit requests. It never
// calls the CLI and stops after shutdown.
func (a *menuApp) loopRender() {
	ticker := time.NewTicker(uiTick)
	defer ticker.Stop()
	for {
		select {
		case <-a.stopUI:
			return
		case <-a.forceRender:
		case <-ticker.C:
		}
		if !a.uiAlive() {
			return
		}
		a.render()
	}
}

// loopLoadStatus requests a redraw once per second while a load runs so the
// elapsed-time line advances.
func (a *menuApp) loopLoadStatus() {
	ticker := time.NewTicker(loadTick)
	defer ticker.Stop()
	for {
		select {
		case <-a.stopUI:
			return
		case <-ticker.C:
		}
		if _, running := a.load.Running(); running {
			a.requestRender()
		}
	}
}

func (a *menuApp) requestRender() {
	if a.ctrl == nil {
		return
	}
	select {
	case a.forceRender <- struct{}{}:
	default:
	}
}

// panelOpened runs when the popover opens: request a low-rate refresh and
// re-render cached data immediately. It never loads or unloads a model.
func (a *menuApp) panelOpened() {
	if a.poller != nil {
		a.poller.Request()
	}
	a.loadProfilesAsync(false)
	a.requestRender()
}

// panelRefresh is the Refresh status row / manual refresh.
func (a *menuApp) panelRefresh() {
	a.refreshAsync()
	a.loadProfilesAsync(true)
	a.requestRender()
}

func (a *menuApp) refreshAsync() {
	if a.poller == nil {
		return
	}
	go func() {
		a.poller.RefreshNow(context.Background())
		a.requestRender()
	}()
}

// panelPickerOpen opens the cached profile picker.
func (a *menuApp) panelPickerOpen() {
	a.uiMu.Lock()
	if a.done || a.ctrl == nil {
		a.uiMu.Unlock()
		return
	}
	a.picking = true
	a.uiMu.Unlock()
	a.requestRender()
}

// panelPickerClose returns to the overview. It never loads or cancels a load.
func (a *menuApp) panelPickerClose() {
	a.uiMu.Lock()
	if a.done {
		a.uiMu.Unlock()
		return
	}
	a.picking = false
	a.uiMu.Unlock()
	a.requestRender()
}

// panelChoose reserves one switch and previews the changes off the UI thread.
func (a *menuApp) panelChoose(id string) {
	a.uiMu.Lock()
	if a.done || a.ctrl == nil {
		a.uiMu.Unlock()
		return
	}
	a.picking = false
	accepted := false
	for _, choice := range a.profiles {
		if choice.ID == id {
			accepted = a.load.Start(id)
			break
		}
	}
	if accepted {
		a.profileResult = ""
	}
	a.uiMu.Unlock()
	if accepted {
		go a.switchProfile(id)
	}
	a.render()
}

func (a *menuApp) switchProfile(name string) {
	var result string
	var err error
	defer func() {
		a.uiMu.Lock()
		if err != nil {
			result = menubarui.FriendlyError(err)
		}
		a.profileResult = result
		a.load.Finish(err)
		a.uiMu.Unlock()
		a.requestRender()
		a.refreshAsync()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	plan, err := a.ctrl.PlanProfile(ctx, name)
	cancel()
	if err != nil || !a.uiAlive() || !panelConfirmProfile(name, plan.Details) || !a.uiAlive() {
		return
	}
	err = a.ctrl.ApplyProfile(context.Background(), name, plan.ID)
	if err == nil {
		result = "Applied profile " + name
	}
}

func (a *menuApp) panelToggleLogin() {
	enable := !menubarui.LoginEnabled()
	if err := menubarui.SetLogin(enable); err != nil {
		return
	}
	a.uiMu.Lock()
	if !a.done {
		a.prefs.StartAtLogin = enable
	}
	prefs := a.prefs
	a.uiMu.Unlock()
	_ = menubarui.SavePrefs(prefs)
	a.requestRender()
}

func (a *menuApp) panelToggleTitle() {
	a.uiMu.Lock()
	if a.done {
		a.uiMu.Unlock()
		return
	}
	a.prefs.ShowTitle = !a.prefs.ShowTitle
	prefs := a.prefs
	a.uiMu.Unlock()
	_ = menubarui.SavePrefs(prefs)
	a.requestRender()
}

// panelCopyDiagnostics captures a report even when the controller is missing.
// The capture and pbcopy run away from AppKit's event thread with a bounded
// wait so a slow clipboard write cannot freeze the UI.
func (a *menuApp) panelCopyDiagnostics() {
	a.uiMu.Lock()
	prefs := a.prefs
	catalogErr := a.catalogErr
	a.uiMu.Unlock()

	go func() {
		var snap menubarui.Snapshot
		if a.ctrl != nil {
			snap = a.ctrl.Snapshot()
		} else {
			snap.Error = errText(a.cliErr)
		}
		lastRefresh := "not yet"
		if a.poller != nil {
			lastRefresh = a.poller.LastRefresh()
		}
		d := menubarui.Diagnostics{
			Version:     a.version,
			CLIPath:     a.cli,
			CLIError:    errText(a.cliErr),
			Prefs:       prefs,
			Login:       menubarui.LoginEnabled(),
			Snapshot:    snap,
			LastRefresh: lastRefresh,
		}
		text := menubarui.DiagnosticsText(d)
		if catalogErr != "" {
			text += "Profile list unavailable (" + catalogErr + ")\n"
		}
		ctx, cancel := context.WithTimeout(context.Background(), pbcopyBound)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/usr/bin/pbcopy")
		cmd.Stdin = strings.NewReader(text)
		_ = cmd.Run()
	}()
}

// loadProfilesAsync coalesces fetches and records failures so the picker and
// diagnostics can show a real error instead of a silent empty list.
func (a *menuApp) loadProfilesAsync(force bool) {
	a.uiMu.Lock()
	if a.ctrl == nil || a.done || a.catalogBusy || (!force && time.Since(a.catalogAt) < catalogFresh) {
		a.uiMu.Unlock()
		return
	}
	a.catalogBusy = true
	a.uiMu.Unlock()
	a.requestRender()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), catalogBound)
		defer cancel()
		profiles, err := a.ctrl.Profiles(ctx)
		a.uiMu.Lock()
		a.catalogBusy = false
		a.catalogAt = time.Now()
		done := a.done
		if err != nil {
			a.catalogErr = menubarui.FriendlyError(err)
		} else {
			a.profiles = profiles
			a.catalogErr = ""
		}
		a.uiMu.Unlock()
		if done {
			return
		}
		a.requestRender()
	}()
}

// panelQuit stops presentation and quits, but never cancels an accepted load:
// it waits for the CLI child to finish before terminating the app.
func (a *menuApp) panelQuit() {
	a.uiMu.Lock()
	if a.done {
		a.uiMu.Unlock()
		return
	}
	a.done = true
	running := a.load.AnyRunning()
	a.uiMu.Unlock()

	a.stopOnce.Do(func() {
		if a.stopUI != nil {
			close(a.stopUI)
		}
	})

	if running {
		// Hold the single-instance lock until the accepted load finishes, then
		// release it only at actual termination. The load is never cancelled.
		go func() {
			waitAnyLoadDone(&a.load)
			a.releaseLock()
			bridgeQuitApp()
		}()
		return
	}
	a.releaseLock()
	bridgeQuitApp()
}

// releaseLock drops the single-instance lock once, at final termination. The
// kernel would also release it on process exit; releasing explicitly here just
// makes handoff to the next instance immediate and unambiguous.
func (a *menuApp) releaseLock() {
	a.uiMu.Lock()
	lock := a.lock
	a.lock = nil
	a.uiMu.Unlock()
	if lock != nil {
		lock.Release()
	}
}

func waitAnyLoadDone(t *menubarui.LoadTracker) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for range tick.C {
		if !t.AnyRunning() {
			return
		}
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return menubarui.FriendlyError(err)
}
