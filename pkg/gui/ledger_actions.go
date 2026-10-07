package gui

import (
	"github.com/jesseduffield/lazydocker/pkg/commands"
	"github.com/jesseduffield/lazydocker/pkg/ledger"
)

// runTrackedCommand runs a non-interactive shell command through the
// subprocess execution path, recording raw command text, exit code, object and
// timestamps in the ledger. It replaces direct gui.OSCommand.RunCommand call
// sites for docker object operations. When the ledger is disabled the
// behaviour is identical to gui.OSCommand.RunCommand.
func (gui *Gui) runTrackedCommand(action string, target ledger.Target, command string) error {
	op := gui.Ledger.Start(ledger.PathProcess, action).For(target)
	exitCode, err := gui.OSCommand.RunCommandWithExitCode(command)
	return op.FinishProcess(command, exitCode, err)
}

// runTrackedCommandForObject is runTrackedCommand with the target derived from
// a custom-command context (container/service/image/volume/network/project).
func (gui *Gui) runTrackedCommandForObject(action string, obj commands.CommandObject, command string) error {
	return gui.runTrackedCommand(action, commands.CommandObjectLedgerTarget(obj), command)
}

// newLedgerBatchID starts a new batch group. Returns "" when the ledger is
// disabled.
func (gui *Gui) newLedgerBatchID() string {
	return gui.DockerCommand.Ledger().NewBatchID()
}

// projectLedgerTarget resolves the ledger target for a project action,
// falling back to the local compose project when no project was selected.
func (gui *Gui) projectLedgerTarget(project *commands.Project) ledger.Target {
	name := ""
	if project != nil {
		name = project.Name
	}
	if name == "" {
		name = gui.DockerCommand.LocalProjectName
	}
	return commands.ProjectLedgerTarget(name)
}
