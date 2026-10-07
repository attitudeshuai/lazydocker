package gui

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/client"

	"github.com/go-errors/errors"

	throttle "github.com/boz/go-throttle"
	"github.com/jesseduffield/gocui"
	lcUtils "github.com/jesseduffield/lazycore/pkg/utils"
	"github.com/jesseduffield/lazydocker/pkg/commands"
	"github.com/jesseduffield/lazydocker/pkg/config"
	"github.com/jesseduffield/lazydocker/pkg/gui/panels"
	"github.com/jesseduffield/lazydocker/pkg/gui/types"
	"github.com/jesseduffield/lazydocker/pkg/i18n"
	"github.com/jesseduffield/lazydocker/pkg/tasks"
	"github.com/jesseduffield/lazydocker/pkg/utils"
	"github.com/sasha-s/go-deadlock"
	"github.com/sirupsen/logrus"
)

// Gui wraps the gocui Gui object which handles rendering and events
type Gui struct {
	g             *gocui.Gui
	Log           *logrus.Entry
	DockerCommand *commands.DockerCommand
	OSCommand     *commands.OSCommand
	State         guiState
	Config        *config.AppConfig
	Tr            *i18n.TranslationSet
	statusManager *statusManager
	taskManager   *tasks.TaskManager
	ErrorChan     chan error
	Views         Views

	// if we've suspended the gui (e.g. because we've switched to a subprocess)
	// we typically want to pause some things that are running like background
	// file refreshes
	PauseBackgroundThreads bool

	Mutexes

	Panels Panels

	// Everything tied to the ACTIVE docker connection (event stream, per-
	// container stats streams) derives from connectionCtx. Cancelling it aborts
	// those streams; loopWG covers the owner goroutines, statsWG the per-
	// container monitor goroutines. Used for both shutdown and connection switches.
	connectionCtx    context.Context
	cancelConnection context.CancelFunc
	loopWG           sync.WaitGroup
	statsWG          sync.WaitGroup

	// refreshTrigger invokes the throttled full refresh; stored so restarted
	// background loops after a connection switch can trigger it.
	refreshTrigger func()

	// refreshMutex is held (RLock) while refresh goroutines are running; a
	// connection switch takes it exclusively so no in-flight refresh from the
	// old daemon can land its results during or after the swap.
	refreshMutex sync.RWMutex

	// switchMutex serialises connection switches so two can't overlap.
	switchMutex sync.Mutex
}

type Panels struct {
	Projects   *panels.SideListPanel[*commands.Project]
	Services   *panels.SideListPanel[*commands.Service]
	Containers *panels.SideListPanel[*commands.Container]
	Images     *panels.SideListPanel[*commands.Image]
	Volumes    *panels.SideListPanel[*commands.Volume]
	Networks   *panels.SideListPanel[*commands.Network]
	Menu       *panels.SideListPanel[*types.MenuItem]
}

type Mutexes struct {
	SubprocessMutex deadlock.Mutex
	ViewStackMutex  deadlock.Mutex
}

type mainPanelState struct {
	// ObjectKey tells us what context we are in. For example, if we are looking at the logs of a particular service in the services panel this key might be 'services-<service id>-logs'. The key is made so that if something changes which might require us to re-run the logs command or run a different command, the key will be different, and we'll then know to do whatever is required. Object key probably isn't the best name for this but Context is already used to refer to tabs. Maybe I should just call them tabs.
	ObjectKey string
}

type panelStates struct {
	Main *mainPanelState
}

type guiState struct {
	// the names of views in the current focus stack (last item is the current view)
	ViewStack        []string
	Platform         commands.Platform
	Panels           *panelStates
	SubProcessOutput string
	Stats            map[string]commands.ContainerStats

	// if true, we show containers with an 'exited' status in the containers panel
	ShowExitedContainers bool

	ScreenMode WindowMaximisation

	// Maintains the state of manual filtering i.e. typing in a substring
	// to filter on in the current panel.
	Filter filterState
}

type filterState struct {
	// If true then we're either currently inside the filter view
	// or we've committed the filter and we're back in the list view
	active bool
	// The panel that we're filtering.
	panel panels.ISideListPanel
	// The string that we're filtering on
	needle string
}

// screen sizing determines how much space your selected window takes up (window
// as in panel, not your terminal's window). Sometimes you want a bit more space
// to see the contents of a panel, and this keeps track of how much maximisation
// you've set
type WindowMaximisation int

const (
	SCREEN_NORMAL WindowMaximisation = iota
	SCREEN_HALF
	SCREEN_FULL
)

func getScreenMode(config *config.AppConfig) WindowMaximisation {
	switch config.UserConfig.Gui.ScreenMode {
	case "normal":
		return SCREEN_NORMAL
	case "half":
		return SCREEN_HALF
	case "fullscreen":
		return SCREEN_FULL
	default:
		return SCREEN_NORMAL
	}
}

// NewGui builds a new gui handler
func NewGui(log *logrus.Entry, dockerCommand *commands.DockerCommand, oSCommand *commands.OSCommand, tr *i18n.TranslationSet, config *config.AppConfig, errorChan chan error) (*Gui, error) {
	initialState := guiState{
		Platform: *oSCommand.Platform,
		Panels: &panelStates{
			Main: &mainPanelState{
				ObjectKey: "",
			},
		},
		ViewStack: []string{},

		ShowExitedContainers: true,
		ScreenMode:           getScreenMode(config),
	}

	gui := &Gui{
		Log:           log,
		DockerCommand: dockerCommand,
		OSCommand:     oSCommand,
		State:         initialState,
		Config:        config,
		Tr:            tr,
		statusManager: &statusManager{},
		taskManager:   tasks.NewTaskManager(log, tr),
		ErrorChan:     errorChan,
	}

	deadlock.Opts.Disable = !gui.Config.Debug
	deadlock.Opts.DeadlockTimeout = 10 * time.Second

	return gui, nil
}

func (gui *Gui) renderGlobalOptions() error {
	return gui.renderOptionsMap(map[string]string{
		"PgUp/PgDn": gui.Tr.Scroll,
		"← → ↑ ↓":   gui.Tr.Navigate,
		"q":         gui.Tr.Quit,
		"b":         gui.Tr.ViewBulkCommands,
		"x":         gui.Tr.Menu,
	})
}

func (gui *Gui) goEvery(interval time.Duration, function func() error) {
	_ = function() // time.Tick doesn't run immediately so we'll do that here // TODO: maybe change
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			if !gui.PauseBackgroundThreads {
				_ = function()
			}
		}
	}()
}

// Run setup the gui with keybindings and start the mainloop
func (gui *Gui) Run() error {
	// closing our task manager which in turn closes the current task if there is any, so we aren't leaving processes lying around after closing lazydocker
	defer gui.taskManager.Close()

	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode:       gocui.OutputTrue,
		RuneReplacements: map[rune]string{},
	})
	if err != nil {
		return err
	}
	defer g.Close()

	// forgive the double-negative, this is because of my yaml `omitempty` woes
	if !gui.Config.UserConfig.Gui.IgnoreMouseEvents {
		g.Mouse = true
	}

	gui.g = g // TODO: always use gui.g rather than passing g around everywhere

	// if the deadlock package wants to report a deadlock, we first need to
	// close the gui so that we can actually read what it prints.
	deadlock.Opts.LogBuf = lcUtils.NewOnceWriter(os.Stderr, func() {
		gui.g.Close()
	})

	if err := gui.SetColorScheme(); err != nil {
		return err
	}

	throttledRefresh := throttle.ThrottleFunc(time.Millisecond*50, true, gui.refresh)
	defer throttledRefresh.Stop()

	go func() {
		for err := range gui.ErrorChan {
			if err == nil {
				continue
			}
			if strings.Contains(err.Error(), "No such container") {
				// this happens all the time when e.g. restarting containers so we won't worry about it
				gui.Log.Warn(err)
				continue
			}
			_ = gui.createErrorPanel(err.Error())
		}
	}()

	g.SetManager(gocui.ManagerFunc(gui.layout), gocui.ManagerFunc(gui.getFocusLayout()))

	if err := gui.createAllViews(); err != nil {
		return err
	}
	if err := gui.setInitialViewContent(); err != nil {
		return err
	}

	// TODO: see if we can avoid the circular dependency
	gui.setPanels()

	if err = gui.keybindings(g); err != nil {
		return err
	}

	if gui.g.CurrentView() == nil {
		viewName := gui.initiallyFocusedViewName()
		view, err := gui.g.View(viewName)
		if err != nil {
			return err
		}

		if err := gui.switchFocus(view); err != nil {
			return err
		}
	}

	gui.refreshTrigger = throttledRefresh.Trigger
	gui.startBackgroundLoops()
	defer gui.stopBackgroundLoops()

	go func() {
		throttledRefresh.Trigger()

		gui.goEvery(time.Millisecond*30, gui.reRenderMain)
		gui.goEvery(time.Millisecond*1000, gui.updateContainerDetails)
		gui.goEvery(time.Millisecond*1000, gui.checkForContextChange)
		// we need to regularly re-render these because their stats will be changed in the background
		gui.goEvery(time.Millisecond*1000, gui.renderContainersAndServices)
	}()

	err = g.MainLoop()
	if err == gocui.ErrQuit {
		return nil
	}
	return err
}

func (gui *Gui) setPanels() {
	gui.Panels = Panels{
		Projects:   gui.getProjectPanel(),
		Services:   gui.getServicesPanel(),
		Containers: gui.getContainersPanel(),
		Images:     gui.getImagesPanel(),
		Volumes:    gui.getVolumesPanel(),
		Networks:   gui.getNetworksPanel(),
		Menu:       gui.getMenuPanel(),
	}
}

func (gui *Gui) updateContainerDetails() error {
	return gui.DockerCommand.RefreshContainerDetails(gui.Panels.Containers.List.GetAllItems())
}

func (gui *Gui) refresh() {
	runRefresh := func(f func() error) {
		go func() {
			gui.refreshMutex.RLock()
			defer gui.refreshMutex.RUnlock()

			if err := f(); err != nil {
				gui.Log.Error(err)
			}
		}()
	}

	// Refresh containers/services first, then projects (which depend on
	// container labels to discover projects).
	runRefresh(func() error {
		if err := gui.refreshContainersAndServices(); err != nil {
			return err
		}
		return gui.refreshProject()
	})
	runRefresh(gui.reloadVolumes)
	runRefresh(gui.reloadNetworks)
	runRefresh(gui.reloadImages)
}

// startBackgroundLoops creates the cancellable context bound to the active
// connection and launches the event listener and stats collector on it.
func (gui *Gui) startBackgroundLoops() {
	ctx, cancel := context.WithCancel(context.Background())
	gui.connectionCtx = ctx
	gui.cancelConnection = cancel

	gui.loopWG.Add(2)
	go func() {
		defer gui.loopWG.Done()
		gui.listenForEvents(ctx, gui.refreshTrigger)
	}()
	go func() {
		defer gui.loopWG.Done()
		gui.monitorContainerStats(ctx)
	}()
}

// stopBackgroundLoops cancels every stream that belongs to the active
// connection and waits for the owning goroutines — first the two loops (so no
// new stats goroutine can be spawned), then the per-container stat monitors.
// Used on both shutdown and connection switch.
func (gui *Gui) stopBackgroundLoops() {
	if gui.cancelConnection != nil {
		gui.cancelConnection()
	}
	gui.loopWG.Wait()
	gui.statsWG.Wait()
}

// handleSwitchConnectionPrompt asks for a host URL or context name and, on
// confirmation, performs a verified runtime connection switch.
func (gui *Gui) handleSwitchConnectionPrompt(g *gocui.Gui, v *gocui.View) error {
	return gui.createPromptPanel(gui.Tr.SwitchConnectionTitle, func(g *gocui.Gui, v *gocui.View) error {
		target := gui.trimmedContent(v)
		return gui.WithWaitingStatus(gui.Tr.SwitchingConnectionStatus, func() error {
			return gui.switchConnection(target)
		})
	})
}

// switchConnection is the runtime connection-switch pathway:
//  1. establish the candidate and verify it with a ping — on failure the live
//     connection is untouched and the reason is returned;
//  2. quiesce the app (background loops, event/stats streams, main-panel task,
//     in-flight refreshes);
//  3. atomically replace client, tunnel and the handles stored on resource
//     objects and clear lists so no old-daemon row can remain;
//  4. explicitly release the old connection;
//  5. rebuild subscriptions/collectors on the new connection and fully refresh.
func (gui *Gui) switchConnection(target string) error {
	gui.switchMutex.Lock()
	defer gui.switchMutex.Unlock()

	// 1. Bring up and verify before touching anything live.
	candidate, err := gui.DockerCommand.PrepareConnection(target)
	if err != nil {
		return err
	}

	// 2. Quiesce everything that talks to the old daemon.
	gui.PauseBackgroundThreads = true
	gui.stopBackgroundLoops()
	gui.taskManager.Close()

	// Drain in-flight refreshes and block any new result from landing while
	// the swap is in progress.
	gui.refreshMutex.Lock()

	// 3. Atomic swap of client/tunnel/environment, then rewire the handles
	// stored on the panel objects and clear the lists.
	previousClosers := gui.DockerCommand.CommitConnection(candidate)
	gui.replaceClientHandles(candidate.Client)
	gui.resetPanelLists()
	gui.State.Panels.Main.ObjectKey = ""
	gui.clearMainView()

	// 4. Explicitly release the old connection (client and ssh tunnel).
	_ = utils.CloseMany(previousClosers)
	gui.refreshMutex.Unlock()

	// 5. Rebuild subscriptions and collectors on the new connection, resume
	// background work and repopulate every panel from the new daemon.
	gui.startBackgroundLoops()
	gui.PauseBackgroundThreads = false
	gui.refreshTrigger()

	// re-run selection so the main panel shows the new daemon immediately
	return gui.newLineFocused(gui.g.CurrentView())
}

// replaceClientHandles updates the docker client handle stored on every
// resource object currently in the panels, so no object can keep talking to
// the daemon we switched away from.
func (gui *Gui) replaceClientHandles(newClient *client.Client) {
	for _, ctr := range gui.Panels.Containers.List.GetAllItems() {
		ctr.Client = newClient
	}
	for _, img := range gui.Panels.Images.List.GetAllItems() {
		img.Client = newClient
	}
	for _, vol := range gui.Panels.Volumes.List.GetAllItems() {
		vol.Client = newClient
	}
	for _, nw := range gui.Panels.Networks.List.GetAllItems() {
		nw.Client = newClient
	}
}

// resetPanelLists empties every side panel (and clamps selection to zero) so
// the old daemon's rows cannot remain; subsequent refreshes repopulate them.
func (gui *Gui) resetPanelLists() {
	gui.Panels.Projects.SetItems(nil)
	gui.Panels.Services.SetItems(nil)
	gui.Panels.Containers.SetItems(nil)
	gui.Panels.Images.SetItems(nil)
	gui.Panels.Volumes.SetItems(nil)
	gui.Panels.Networks.SetItems(nil)
}

func (gui *Gui) listenForEvents(ctx context.Context, refresh func()) {
	errorCount := 0

	onError := func(err error) {
		if err != nil {
			gui.ErrorChan <- errors.Errorf("Docker event stream returned error: %s\nRetry count: %d", err.Error(), errorCount)
		}
		errorCount++
		time.Sleep(time.Second * 2)
	}

outer:
	for {
		if ctx.Err() != nil {
			return
		}

		// bind the actual event stream to ctx: cancelling it (connection
		// switch / shutdown) aborts the HTTP request instead of leaving a
		// live subscription on the old daemon.
		messageChan, errChan := gui.DockerCommand.Client.Events(ctx, events.ListOptions{})

		if errorCount > 0 {
			select {
			case <-ctx.Done():
				return
			case err := <-errChan:
				onError(err)
				continue outer
			default:
				// If we're here then we lost connection to docker and we just got it back.
				// The reason we do this refresh explicitly is because successfully
				// reconnecting with docker does not mean it's going to send us a new
				// event any time soon.

				// Assuming the confirmation prompt currently holds the given error
				_ = gui.closeConfirmationPrompt()
				refresh()
				errorCount = 0
			}
		}

		for {
			select {
			case <-ctx.Done():
				return
			case message := <-messageChan:
				// We could be more granular about what events should trigger which refreshes.
				// At the moment it's pretty efficient though, and it might not be worth
				// the maintenance burden of mapping specific events to specific refreshes
				refresh()

				gui.Log.Infof("received event of type: %s", message.Type)
			case err := <-errChan:
				// the stream ended because we're switching away: leave
				// silently, the new connection gets its own listener
				if ctx.Err() != nil {
					return
				}
				onError(err)
				continue outer
			}
		}
	}
}

// checkForContextChange runs the currently focused panel's 'select' function, simulating the current item having just been selected. This will then trigger a check to see if anything's changed (e.g. a service has a new container) and if so, the appropriate code will run. For example, if you're reading logs from a service and all of a sudden its container changes, this will trigger the 'select' function, which will work out that the context is not different because of the new container, and then it will re-attempt to get the logs, this time for the correct container. This 'context' is stored in the main panel's ObjectKey. I'm using the term 'context' here more broadly than just the different tabs you can view in a panel.
func (gui *Gui) checkForContextChange() error {
	return gui.newLineFocused(gui.g.CurrentView())
}

func (gui *Gui) reRenderMain() error {
	mainView := gui.Views.Main
	if mainView == nil {
		return nil
	}
	if mainView.IsTainted() {
		gui.g.Update(func(g *gocui.Gui) error {
			return nil
		})
	}
	return nil
}

func (gui *Gui) quit(g *gocui.Gui, v *gocui.View) error {
	if gui.Config.UserConfig.ConfirmOnQuit {
		return gui.createConfirmationPanel("", gui.Tr.ConfirmQuit, func(g *gocui.Gui, v *gocui.View) error {
			return gocui.ErrQuit
		}, nil)
	}
	return gocui.ErrQuit
}

// this handler is executed when we press escape when there is only one view
// on the stack.
func (gui *Gui) escape() error {
	if gui.State.Filter.active {
		return gui.clearFilter()
	}

	return nil
}

func (gui *Gui) handleDonate(g *gocui.Gui, v *gocui.View) error {
	if !gui.g.Mouse {
		return nil
	}

	cx, _ := v.Cursor()
	if cx > len(gui.Tr.Donate) {
		return nil
	}
	return gui.OSCommand.OpenLink("https://github.com/sponsors/jesseduffield")
}

func (gui *Gui) editFile(filename string) error {
	cmd, err := gui.OSCommand.EditFile(filename)
	if err != nil {
		return gui.createErrorPanel(err.Error())
	}

	return gui.runSubprocess(cmd)
}

func (gui *Gui) openFile(filename string) error {
	if err := gui.OSCommand.OpenFile(filename); err != nil {
		return gui.createErrorPanel(err.Error())
	}
	return nil
}

func (gui *Gui) handleCustomCommand(g *gocui.Gui, v *gocui.View) error {
	return gui.createPromptPanel(gui.Tr.CustomCommandTitle, func(g *gocui.Gui, v *gocui.View) error {
		command := gui.trimmedContent(v)
		return gui.runSubprocess(gui.OSCommand.RunCustomCommand(command))
	})
}

func (gui *Gui) ShouldRefresh(key string) bool {
	if gui.State.Panels.Main.ObjectKey == key {
		return false
	}

	gui.State.Panels.Main.ObjectKey = key
	return true
}

func (gui *Gui) initiallyFocusedViewName() string {
	if gui.DockerCommand.IsProjectScoped() {
		return "services"
	}
	return "containers"
}

func (gui *Gui) IgnoreStrings() []string {
	return gui.Config.UserConfig.Ignore
}

func (gui *Gui) Update(f func() error) {
	gui.g.Update(func(*gocui.Gui) error { return f() })
}

func (gui *Gui) monitorContainerStats(ctx context.Context) {
	// periodically loop through running containers and see if we need to create a monitor goroutine for any
	// every second we check if we need to spawn a new goroutine
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// snapshot the containers: a switch may clear the list, and stat
			// goroutines are tracked so stopBackgroundLoops can wait for them
			for _, container := range gui.Panels.Containers.List.GetAllItems() {
				if !container.MonitoringStats {
					container := container
					gui.statsWG.Add(1)
					go func() {
						defer gui.statsWG.Done()
						gui.DockerCommand.CreateClientStatMonitor(ctx, container)
					}()
				}
			}
		}
	}
}

// this is used by our cheatsheet code to generate keybindings. We need some views
// and panels to exist for us to know what keybindings there are, so we invoke
// gocui in headless mode and create them.
func (gui *Gui) SetupFakeGui() {
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode:       gocui.OutputTrue,
		RuneReplacements: map[rune]string{},
		Headless:         true,
	})
	if err != nil {
		panic(err)
	}
	gui.g = g
	defer g.Close()
	if err := gui.createAllViews(); err != nil {
		panic(err)
	}

	gui.setPanels()
}
