package gui

import (
	"fmt"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/fatih/color"
	"github.com/jesseduffield/gocui"
	"github.com/jesseduffield/lazydocker/pkg/commands"
	"github.com/jesseduffield/lazydocker/pkg/utils"
)

type guardedDeleteOpts struct {
	target  commands.ObjectRef
	project string
	remove  commands.RemoveFunc
	// onError handles an error from the direct removal; when nil the error is
	// shown in an error panel
	onError func(error) error
	// waiting controls whether the "removing" status is shown on the legacy
	// path. It lets each call site preserve its pre-feature behavior exactly.
	waiting bool
}

// guardedDelete is the single gateway for object deletion. It asks the
// DockerCommand for a pre-deletion judgment and, depending on the resolved
// policy, either performs the exact legacy removal, shows a blocking error,
// asks for confirmation after a warning, or asks for confirmation for a
// layer-by-layer cascade.
func (gui *Gui) guardedDelete(opts guardedDeleteOpts) error {
	plan := gui.DockerCommand.PlanDelete(opts.target, opts.project)

	runDirect := func() error {
		execution := func() error {
			if err := opts.remove(opts.target); err != nil {
				if opts.onError != nil {
					return opts.onError(err)
				}
				return gui.createErrorPanel(err.Error())
			}
			return nil
		}
		if opts.waiting || plan.Action != commands.PolicyLegacy {
			return gui.WithWaitingStatus(gui.Tr.RemovingStatus, execution)
		}
		return execution()
	}

	switch plan.Action {
	case commands.PolicyLegacy:
		return runDirect()

	case commands.PolicyReject:
		return gui.createErrorPanel(
			gui.Tr.DeletionBlockedTitle + "\n\n" + renderPlanJudgment(plan))

	case commands.PolicyWarn:
		return gui.createConfirmationPanel(
			gui.Tr.Confirm,
			gui.Tr.WarnContinuePrompt+"\n\n"+renderPlanJudgment(plan),
			func(g *gocui.Gui, v *gocui.View) error { return runDirect() },
			nil,
		)

	case commands.PolicyCascade:
		return gui.createConfirmationPanel(
			gui.Tr.Confirm,
			gui.Tr.CascadePrompt+"\n\n"+renderCascadePlan(plan),
			func(g *gocui.Gui, v *gocui.View) error {
				return gui.WithWaitingStatus(gui.Tr.RemovingStatus, func() error {
					if err := gui.DockerCommand.ExecuteDelete(plan, gui.cascadeRemove); err != nil {
						return gui.createErrorPanel(err.Error())
					}
					return nil
				})
			},
			nil,
		)
	}

	return nil
}

// cascadeRemove removes a node of the cascade. Only containers can reference
// images, volumes or networks, so they are the only nodes encountered.
func (gui *Gui) cascadeRemove(ref commands.ObjectRef) error {
	if ref.Type != commands.ContainerObject {
		return fmt.Errorf("cascade is unable to remove %s automatically", ref)
	}
	ctr := gui.findContainerByID(ref.ID)
	if ctr == nil {
		return fmt.Errorf("container %s disappeared before it could be removed", ref.ID)
	}
	// Force remove so running containers do not block the cascade. We do not
	// pass RemoveVolumes here: the cascade must not destroy volumes other
	// than the explicit target.
	return ctr.Remove(container.RemoveOptions{Force: true})
}

func (gui *Gui) findContainerByID(id string) *commands.Container {
	for _, ctr := range gui.Panels.Containers.List.GetAllItems() {
		if ctr.ID == id {
			return ctr
		}
	}
	return nil
}

// renderPlanJudgment renders the referrers and uncertainties of a plan.
func renderPlanJudgment(plan commands.DeletePlan) string {
	var sb strings.Builder

	if len(plan.Referrers) > 0 {
		sb.WriteString(utils.ColoredString("Referenced by:", color.FgYellow) + "\n")
		for _, edge := range plan.Referrers {
			sb.WriteString("  - " + renderEdgeLine(edge) + "\n")
		}
	}

	if len(plan.Unknowns) > 0 {
		sb.WriteString(utils.ColoredString("References could not be determined:", color.FgYellow) + "\n")
		for _, u := range plan.Unknowns {
			sb.WriteString("  - " + utils.ColoredString("!", color.FgRed) + " " + u.Reason + "\n")
		}
	}

	return sb.String()
}

// renderCascadePlan renders the removal layers, deepest first, then target.
func renderCascadePlan(plan commands.DeletePlan) string {
	var sb strings.Builder

	for layerIdx := len(plan.Layers) - 1; layerIdx >= 1; layerIdx-- {
		sb.WriteString(fmt.Sprintf("%s:\n", utils.ColoredString(
			fmt.Sprintf("Layer %d (removed first)", layerIdx), color.FgYellow)))
		for _, node := range plan.Layers[layerIdx] {
			sb.WriteString("  - " + node.String() + "\n")
		}
	}

	sb.WriteString(utils.ColoredString("Target:", color.FgYellow) + "\n")
	sb.WriteString("  - " + plan.Target.String() + "\n")

	if len(plan.Unknowns) > 0 {
		sb.WriteString("\n" + utils.ColoredString("References could not be determined:", color.FgYellow) + "\n")
		for _, u := range plan.Unknowns {
			sb.WriteString("  - " + utils.ColoredString("!", color.FgRed) + " " + u.Reason + "\n")
		}
	}

	return sb.String()
}

func renderEdgeLine(edge commands.Reference) string {
	contextParts := make([]string, 0, 3)
	if edge.Project != "" {
		contextParts = append(contextParts, "project: "+edge.Project)
	}
	if edge.Service != "" {
		contextParts = append(contextParts, "service: "+edge.Service)
	}
	if edge.Detail != "" {
		contextParts = append(contextParts, edge.Detail)
	}
	context := ""
	if len(contextParts) > 0 {
		context = " (" + strings.Join(contextParts, ", ") + ")"
	}
	return edge.Referrer.String() + context
}

// renderBatchReport renders the skipped and failed items after a batch
// cleanup.
func renderBatchReport(report commands.BatchReport) string {
	var sb strings.Builder

	if len(report.Skipped) > 0 {
		sb.WriteString(utils.ColoredString("Blocked by policy:", color.FgYellow) + "\n")
		for _, plan := range report.Skipped {
			sb.WriteString("  - " + plan.Target.String() + "\n")
			for _, edge := range plan.Referrers {
				sb.WriteString("      " + renderEdgeLine(edge) + "\n")
			}
			for _, u := range plan.Unknowns {
				sb.WriteString("      " + utils.ColoredString("!", color.FgRed) + " " + u.Reason + "\n")
			}
		}
	}

	if len(report.Failed) > 0 {
		sb.WriteString(utils.ColoredString("Failed:", color.FgYellow) + "\n")
		for _, failure := range report.Failed {
			sb.WriteString(fmt.Sprintf("  - %s: %v\n", failure.Target, failure.Err))
		}
	}

	return sb.String()
}
