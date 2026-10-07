package gui

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jesseduffield/gocui"
	"github.com/jesseduffield/lazydocker/pkg/commands"
	"github.com/jesseduffield/lazydocker/pkg/gui/presentation"
	"github.com/jesseduffield/lazydocker/pkg/gui/types"
	"github.com/jesseduffield/lazydocker/pkg/tasks"
	"github.com/samber/lo"
)

// renderContainerStatsWithHeader renders a container's stats tab, refreshing
// once per second. header (which may be nil) produces the live header shown
// above the graphs, e.g. the collection state or the service level summary.
func (gui *Gui) renderContainerStatsWithHeader(container *commands.Container, header func(*commands.Container) string) tasks.TaskFunc {
	return gui.NewTickerTask(TickerTaskOpts{
		Func: func(ctx context.Context, notifyStopped chan struct{}) {
			headerText := ""
			if header != nil {
				headerText = header(container)
			}

			contents, err := presentation.RenderStats(gui.Config.UserConfig, container, headerText, gui.Views.Main.Width())
			if err != nil {
				_ = gui.createErrorPanel(err.Error())
			}

			gui.reRenderStringMain(contents)
		},
		Duration:   time.Second,
		Before:     func(ctx context.Context) { gui.clearMainView() },
		Wrap:       false, // wrapping looks bad here so we're overriding the config value
		Autoscroll: false,
	})
}

// containerStatsStatusHeader renders the observable collection state of one
// container (running, retrying with backoff details, paused, stopped, ...).
func (gui *Gui) containerStatsStatusHeader(container *commands.Container) string {
	manager := gui.DockerCommand.Stats
	if manager == nil {
		return ""
	}

	status, ok := manager.Status(container.ID)
	if !ok {
		return ""
	}

	return presentation.RenderStatsStatusLine(gui.Tr, status)
}

// serviceStatsHeader renders the aggregate summary across all containers of
// the given service, not just the container linked to the service row.
func (gui *Gui) serviceStatsHeader(service *commands.Service) string {
	all := gui.Panels.Containers.List.GetAllItems()
	scope := commands.ServiceStatsScope(service.ProjectName, service.Name)
	aggregate := gui.DockerCommand.Stats.Aggregate(scope, all)

	title := fmt.Sprintf("%s — %s", service.Name, gui.Tr.StatsSummaryTitle)
	return presentation.RenderAggregate(gui.Tr, title, aggregate)
}

// renderScopeSummary renders just an aggregate summary; used for a service
// without a linked container and for the project stats tab.
func (gui *Gui) renderScopeSummary(build func() (string, commands.StatsScope)) tasks.TaskFunc {
	return gui.NewTickerTask(TickerTaskOpts{
		Func: func(ctx context.Context, notifyStopped chan struct{}) {
			title, scope := build()
			all := gui.Panels.Containers.List.GetAllItems()
			aggregate := gui.DockerCommand.Stats.Aggregate(scope, all)
			gui.reRenderStringMain(presentation.RenderAggregate(gui.Tr, title, aggregate))
		},
		Duration:   time.Second,
		Before:     func(ctx context.Context) { gui.clearMainView() },
		Wrap:       gui.Config.UserConfig.Gui.WrapMainPanel,
		Autoscroll: false,
	})
}

// renderProjectStats renders the project level stats tab: an aggregate across
// every container of the project plus a per-container table.
func (gui *Gui) renderProjectStats(project *commands.Project) tasks.TaskFunc {
	return gui.NewTickerTask(TickerTaskOpts{
		Func: func(ctx context.Context, notifyStopped chan struct{}) {
			all := gui.Panels.Containers.List.GetAllItems()
			scope := commands.ProjectStatsScope(project.Name)

			aggregate := gui.DockerCommand.Stats.Aggregate(scope, all)
			members := lo.Filter(all, func(container *commands.Container, _ int) bool {
				return commands.MatchStatsScope(container, scope)
			})
			sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })

			contents := presentation.RenderProjectStats(gui.Tr, project.Name, aggregate, members)
			gui.reRenderStringMain(contents)
		},
		Duration:   time.Second,
		Before:     func(ctx context.Context) { gui.clearMainView() },
		Wrap:       gui.Config.UserConfig.Gui.WrapMainPanel,
		Autoscroll: false,
	})
}

// statsMenuOperation is one entry in the stats lifecycle menu: an operation
// applied to a particular scope.
type statsMenuOperation struct {
	actionLabel string
	scopeLabel  string
	scope       commands.StatsScope
	apply       func(commands.StatsScope, []*commands.Container) int
}

func (gui *Gui) handleContainersStatsMenu(_ *gocui.Gui, _ *gocui.View) error {
	container, err := gui.Panels.Containers.GetSelectedItem()
	if err != nil {
		return nil
	}

	all := gui.Panels.Containers.List.GetAllItems()
	manager := gui.DockerCommand.Stats

	operations := []statsMenuOperation{}
	for _, operation := range []struct {
		label string
		apply func(commands.StatsScope, []*commands.Container) int
	}{
		{gui.Tr.StatsStart, manager.Start},
		{gui.Tr.StatsPause, manager.Pause},
		{gui.Tr.StatsStop, manager.Stop},
		{gui.Tr.StatsRestart, manager.Restart},
	} {
		operations = append(operations,
			statsMenuOperation{
				actionLabel: operation.label,
				scopeLabel:  gui.Tr.StatsScopeContainer,
				scope:       commands.ContainerStatsScope(container.ID),
				apply:       operation.apply,
			},
			statsMenuOperation{
				actionLabel: operation.label,
				scopeLabel:  gui.Tr.StatsScopeAllContainers,
				scope:       commands.AllStatsScope(),
				apply:       operation.apply,
			},
		)
	}

	return gui.openStatsMenu(operations, all)
}

func (gui *Gui) handleServicesStatsMenu(_ *gocui.Gui, _ *gocui.View) error {
	service, err := gui.Panels.Services.GetSelectedItem()
	if err != nil {
		return nil
	}

	all := gui.Panels.Containers.List.GetAllItems()
	manager := gui.DockerCommand.Stats

	operations := []statsMenuOperation{}
	for _, operation := range []struct {
		label string
		apply func(commands.StatsScope, []*commands.Container) int
	}{
		{gui.Tr.StatsStart, manager.Start},
		{gui.Tr.StatsPause, manager.Pause},
		{gui.Tr.StatsStop, manager.Stop},
		{gui.Tr.StatsRestart, manager.Restart},
	} {
		operations = append(operations,
			statsMenuOperation{
				actionLabel: operation.label,
				scopeLabel:  gui.Tr.StatsScopeService,
				scope:       commands.ServiceStatsScope(service.ProjectName, service.Name),
				apply:       operation.apply,
			},
			statsMenuOperation{
				actionLabel: operation.label,
				scopeLabel:  gui.Tr.StatsScopeProject,
				scope:       commands.ProjectStatsScope(service.ProjectName),
				apply:       operation.apply,
			},
		)
	}

	return gui.openStatsMenu(operations, all)
}

func (gui *Gui) openStatsMenu(operations []statsMenuOperation, all []*commands.Container) error {
	items := make([]*types.MenuItem, len(operations))
	for i, operation := range operations {
		operation := operation
		items[i] = &types.MenuItem{
			LabelColumns: []string{operation.actionLabel, operation.scopeLabel},
			OnPress: func() error {
				operation.apply(operation.scope, all)
				return nil
			},
		}
	}

	return gui.Menu(CreateMenuOptions{
		Title: gui.Tr.StatsMenu,
		Items: items,
	})
}
