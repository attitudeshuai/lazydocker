package gui

import (
	"github.com/fatih/color"
	"github.com/jesseduffield/lazydocker/pkg/commands"
	"github.com/jesseduffield/lazydocker/pkg/config"
	"github.com/jesseduffield/lazydocker/pkg/gui/types"
	"github.com/jesseduffield/lazydocker/pkg/ledger"
	"github.com/jesseduffield/lazydocker/pkg/utils"
	"github.com/samber/lo"
)

func (gui *Gui) createCommandMenu(customCommands []config.CustomCommand, commandObject commands.CommandObject, title string, waitingStatus string, bulk bool) error {
	// One batch id per opened bulk menu: every item chosen from the menu (and
	// every per-object result of a built-in bulk action) stays traceable as
	// part of the same bulk operation. Empty when the ledger is disabled.
	batchID := ""
	if bulk {
		batchID = gui.newLedgerBatchID()
	}

	target := commands.CommandObjectLedgerTarget(commandObject)
	action := "custom-command"
	if bulk {
		action = "bulk-command"
	}

	menuItems := lo.Map(customCommands, func(command config.CustomCommand, idx int) *types.MenuItem {
		resolvedCommand := utils.ApplyTemplate(command.Command, commandObject)

		onPress := func() error {
			if command.InternalFunction != nil {
				// built-in bulk actions (stop/remove/prune all, ...) record
				// their own per-item results and create their own batch id.
				return command.InternalFunction()
			}

			op := gui.Ledger.Start(ledger.PathProcess, action).For(target)
			if bulk {
				op.Batch(batchID, idx, len(customCommands))
			}

			if command.Shell {
				resolvedCommand = gui.OSCommand.NewCommandStringWithShell(resolvedCommand)
			}

			// if we have a command for attaching, we attach and return the subprocess error
			if command.Attach {
				return gui.runSubprocessOperation(op, gui.OSCommand.ExecutableFromString(resolvedCommand))
			}

			return gui.WithWaitingStatus(waitingStatus, func() error {
				exitCode, err := gui.OSCommand.RunCommandWithExitCode(resolvedCommand)
				if err := op.FinishProcess(resolvedCommand, exitCode, err); err != nil {
					return gui.createErrorPanel(err.Error())
				}
				return nil
			})
		}

		return &types.MenuItem{
			LabelColumns: []string{
				command.Name,
				utils.ColoredString(utils.WithShortSha(resolvedCommand), color.FgCyan),
			},
			OnPress: onPress,
		}
	})

	return gui.Menu(CreateMenuOptions{
		Title: title,
		Items: menuItems,
	})
}

func (gui *Gui) createCustomCommandMenu(customCommands []config.CustomCommand, commandObject commands.CommandObject) error {
	return gui.createCommandMenu(customCommands, commandObject, gui.Tr.CustomCommandTitle, gui.Tr.RunningCustomCommandStatus, false)
}

func (gui *Gui) createBulkCommandMenu(customCommands []config.CustomCommand, commandObject commands.CommandObject) error {
	return gui.createCommandMenu(customCommands, commandObject, gui.Tr.BulkCommandTitle, gui.Tr.RunningBulkCommandStatus, true)
}
