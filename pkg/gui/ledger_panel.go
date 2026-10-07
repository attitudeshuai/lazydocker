package gui

import (
	"fmt"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/jesseduffield/lazydocker/pkg/ledger"
	"github.com/jesseduffield/lazydocker/pkg/utils"
)

// ledgerViewLimit caps how many recent entries the in-app ledger view renders.
const ledgerViewLimit = 200

// handleViewLedger renders the operation ledger into the main panel. When a
// side panel object is selected, the view is scoped to that object (or to the
// selected project); otherwise the most recent entries across every object
// are shown.
func (gui *Gui) handleViewLedger() error {
	if !gui.Ledger.Enabled() {
		return gui.createErrorPanel(gui.Tr.LedgerDisabled)
	}

	filter, scope := gui.ledgerFilterForCurrentView()
	entries := gui.Ledger.Query(filter)
	if len(entries) > ledgerViewLimit {
		entries = entries[len(entries)-ledgerViewLimit:]
	}

	gui.RenderStringMain(gui.renderLedgerEntries(entries, scope))
	return nil
}

// ledgerFilterForCurrentView builds the query filter implied by the currently
// focused side panel. Returns an empty filter when no object is selected.
func (gui *Gui) ledgerFilterForCurrentView() (ledger.Filter, string) {
	switch gui.currentViewName() {
	case "containers":
		if ctr, err := gui.Panels.Containers.GetSelectedItem(); err == nil {
			return ledger.Filter{ObjectKind: ledger.ObjectContainer, ObjectID: ctr.ID},
				fmt.Sprintf("%s: %s", ledger.ObjectContainer, ctr.Name)
		}
	case "images":
		if img, err := gui.Panels.Images.GetSelectedItem(); err == nil {
			return ledger.Filter{ObjectKind: ledger.ObjectImage, ObjectID: img.ID},
				fmt.Sprintf("%s: %s", ledger.ObjectImage, img.Name)
		}
	case "volumes":
		if vol, err := gui.Panels.Volumes.GetSelectedItem(); err == nil {
			return ledger.Filter{ObjectKind: ledger.ObjectVolume, ObjectID: vol.Name},
				fmt.Sprintf("%s: %s", ledger.ObjectVolume, vol.Name)
		}
	case "networks":
		if nw, err := gui.Panels.Networks.GetSelectedItem(); err == nil {
			return ledger.Filter{ObjectKind: ledger.ObjectNetwork, ObjectID: nw.Name},
				fmt.Sprintf("%s: %s", ledger.ObjectNetwork, nw.Name)
		}
	case "services":
		if svc, err := gui.Panels.Services.GetSelectedItem(); err == nil {
			// Project + service matches both the service-level compose
			// operations and API calls against the service's containers.
			return ledger.Filter{Project: svc.ProjectName, Service: svc.Name},
				fmt.Sprintf("%s: %s/%s", ledger.ObjectService, svc.ProjectName, svc.Name)
		}
	case "project":
		if project, err := gui.Panels.Projects.GetSelectedItem(); err == nil {
			return ledger.Filter{Project: project.Name},
				fmt.Sprintf("%s: %s", ledger.ObjectProject, project.Name)
		}
	}
	return ledger.Filter{}, ""
}

func (gui *Gui) renderLedgerEntries(entries []ledger.Entry, scope string) string {
	var b strings.Builder

	title := gui.Tr.LedgerTitle
	b.WriteString(utils.ColoredString(title, color.FgGreen))
	b.WriteString("\n")
	if scope != "" {
		b.WriteString(gui.Tr.LedgerScoped + " — " + scope)
		b.WriteString("\n")
	}
	if len(entries) == 0 {
		b.WriteString(gui.Tr.LedgerEmpty)
		return b.String()
	}

	b.WriteString(fmt.Sprintf("%d %s\n\n", len(entries), pluralEntries(len(entries))))

	for _, e := range entries {
		b.WriteString(formatLedgerEntry(e))
		b.WriteString("\n")
	}
	return b.String()
}

func pluralEntries(n int) string {
	if n == 1 {
		return "entry"
	}
	return "entries"
}

func formatLedgerEntry(e ledger.Entry) string {
	var line strings.Builder

	status := utils.ColoredString(" OK  ", color.FgGreen)
	if !e.Success {
		status = utils.ColoredString("FAIL ", color.FgRed)
	}
	line.WriteString(e.StartedAt.Local().Format("2006-01-02 15:04:05"))
	line.WriteString("  ")
	line.WriteString(status)
	line.WriteString(" ")
	line.WriteString(fmt.Sprintf("%-7s", e.Path))
	line.WriteString(" ")
	line.WriteString(fmt.Sprintf("%-22s", e.Action))

	object := objectLabel(e.Target)
	line.WriteString(" ")
	line.WriteString(utils.ColoredString(fmt.Sprintf("%-28s", object), color.FgYellow))

	if e.Target.Project != "" {
		line.WriteString(" ")
		line.WriteString(utils.ColoredString(e.Target.Project, color.FgCyan))
	}

	line.WriteString(" ")
	line.WriteString(fmt.Sprintf("%6s", formatDuration(e.DurationMS)))

	if e.ExitCode != nil {
		line.WriteString("  exit=")
		line.WriteString(fmt.Sprintf("%d", *e.ExitCode))
	}

	if e.BatchID != "" {
		line.WriteString("  batch=")
		line.WriteString(utils.WithShortSha(e.BatchID))
		if e.BatchTotal > 0 {
			line.WriteString(fmt.Sprintf("#%d/%d", e.BatchIndex+1, e.BatchTotal))
		}
	}

	if e.Command != "" {
		line.WriteString("\n         ")
		line.WriteString(utils.ColoredString("$ "+truncate(e.Command, 200), color.FgBlue))
	}
	if e.Error != "" {
		line.WriteString("\n         ")
		line.WriteString(utils.ColoredString("error: "+truncate(e.Error, 200), color.FgRed))
	}

	return line.String()
}

func objectLabel(t ledger.Target) string {
	switch {
	case t.Kind == "":
		return "-"
	case t.Name != "" && t.Name != t.ID:
		return fmt.Sprintf("%s:%s", t.Kind, t.Name)
	default:
		id := t.ID
		if id == "" {
			id = t.Name
		}
		return fmt.Sprintf("%s:%s", t.Kind, utils.WithShortSha(id))
	}
}

func formatDuration(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return (time.Duration(ms) * time.Millisecond).Round(time.Millisecond).String()
}

func truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
