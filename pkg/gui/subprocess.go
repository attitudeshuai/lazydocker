package gui

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	"github.com/fatih/color"
	"github.com/jesseduffield/lazydocker/pkg/commands"
	"github.com/jesseduffield/lazydocker/pkg/ledger"
	"github.com/jesseduffield/lazydocker/pkg/utils"
)

func (gui *Gui) runSubprocess(cmd *exec.Cmd) error {
	return gui.runSubprocessCore(cmd, "", nil)
}

func (gui *Gui) runSubprocessWithMessage(cmd *exec.Cmd, msg string) error {
	return gui.runSubprocessCore(cmd, msg, nil)
}

// runSubprocessTracked runs an interactive subprocess and records it in the
// ledger (raw command text, exit code, object, timestamps, connection).
func (gui *Gui) runSubprocessTracked(action string, target ledger.Target, cmd *exec.Cmd) error {
	return gui.runSubprocessTrackedWithMessage(action, target, cmd, "")
}

// runSubprocessTrackedWithMessage is the tracked variant with an extra hint
// message (e.g. the detach shortcut).
func (gui *Gui) runSubprocessTrackedWithMessage(action string, target ledger.Target, cmd *exec.Cmd, msg string) error {
	op := gui.Ledger.Start(ledger.PathProcess, action).For(target)
	return gui.runSubprocessCore(cmd, msg, op)
}

// runSubprocessOperation runs an interactive subprocess against an operation
// the caller configured itself (used by bulk/custom command menus so the
// record carries the batch id).
func (gui *Gui) runSubprocessOperation(op *ledger.Operation, cmd *exec.Cmd) error {
	return gui.runSubprocessCore(cmd, "", op)
}

func (gui *Gui) runSubprocessCore(cmd *exec.Cmd, msg string, op *ledger.Operation) error {
	gui.Mutexes.SubprocessMutex.Lock()
	defer gui.Mutexes.SubprocessMutex.Unlock()

	if err := gui.g.Suspend(); err != nil {
		return gui.createErrorPanel(err.Error())
	}

	gui.PauseBackgroundThreads = true

	recordErr := gui.runCommand(cmd, msg, op)

	if err := gui.g.Resume(); err != nil {
		return gui.createErrorPanel(err.Error())
	}

	gui.PauseBackgroundThreads = false

	// A ledger write failure must be surfaced rather than silently lost.
	if recordErr != nil {
		return gui.createErrorPanel(recordErr.Error())
	}

	return nil
}

func (gui *Gui) runCommand(cmd *exec.Cmd, msg string, op *ledger.Operation) error {
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stdout
	cmd.Stdin = os.Stdin

	stop := make(chan os.Signal, 1)
	defer signal.Stop(stop)

	go func() {
		signal.Notify(stop, os.Interrupt)
		<-stop

		if err := gui.OSCommand.Kill(cmd); err != nil {
			gui.Log.Error(err)
		}
	}()

	fmt.Fprintf(os.Stdout, "\n%s\n\n", utils.ColoredString("+ "+strings.Join(cmd.Args, " "), color.FgBlue))
	if msg != "" {
		fmt.Fprintf(os.Stdout, "\n%s\n\n", utils.ColoredString(msg, color.FgGreen))
	}
	runErr := cmd.Run()
	if runErr != nil {
		// not handling the error explicitly because usually we're going to see it
		// in the output anyway
		gui.Log.Error(runErr)
	}

	// Record as soon as the process exits (before prompting to return) so the
	// end timestamp reflects the actual process lifetime. We only surface the
	// ledger's persistence error here: a failed attached command itself is
	// visible in the terminal output and was historically not turned into an
	// error panel.
	var recordErr error
	if op != nil {
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		if code := commands.ExitCode(runErr); code != -1 {
			exitCode = code
		}
		recordErr = op.RecordProcessResult(strings.Join(cmd.Args, " "), exitCode, runErr)
		if recordErr != nil {
			gui.Log.Error(recordErr)
		}
	}

	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	gui.promptToReturn()

	return recordErr
}
