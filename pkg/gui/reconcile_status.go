package gui

import (
	"time"

	"github.com/jesseduffield/gocui"
	"github.com/jesseduffield/lazydocker/pkg/utils"
)

// renderReconcileStatuses writes each panel's reconciliation state into its
// view subtitle: whether it is currently fetching, waiting for a
// reconciliation (stale) and when its last successful fetch was. This lets
// the user tell at any moment which panels can be trusted as fresh.
func (gui *Gui) renderReconcileStatuses() error {
	if gui.reconcile == nil || gui.g == nil {
		return nil
	}

	statuses := gui.reconcile.Statuses()

	gui.g.Update(func(*gocui.Gui) error {
		for _, status := range statuses {
			subtitle := gui.reconcileSubtitle(status)
			for _, view := range status.views {
				if view != nil {
					view.Subtitle = subtitle
				}
			}
		}
		return nil
	})

	return nil
}

func (gui *Gui) reconcileSubtitle(status unitStatus) string {
	timestamp := func(t time.Time) string {
		return t.Format("15:04:05")
	}

	switch status.state {
	case stateReconciling:
		return utils.Loader() + " " + gui.Tr.ReconcileReconciling
	case stateFresh:
		return gui.Tr.ReconcileSynced + " " + timestamp(status.lastSuccess)
	default:
		if status.lastSuccess.IsZero() {
			// never successfully fetched yet
			return gui.Tr.ReconcileLoading
		}
		// marked stale (pending event, stream outage, failed fetch): show
		// when the data we're still displaying was last known to be fresh
		return gui.Tr.ReconcilePending + " " + timestamp(status.lastSuccess)
	}
}
