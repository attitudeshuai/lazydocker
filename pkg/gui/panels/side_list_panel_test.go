package panels

import (
	"context"
	"testing"

	"github.com/jesseduffield/gocui"
	"github.com/jesseduffield/lazydocker/pkg/tasks"
	"github.com/stretchr/testify/assert"
)

// fakeGuiForSetItems implements just enough of IGui for SetItems / FilterAndSort.
type fakeGuiForSetItems struct{}

func (fake *fakeGuiForSetItems) HandleClick(v *gocui.View, itemCount int, selectedLine *int, handleSelect func() error) error {
	return nil
}

func (fake *fakeGuiForSetItems) NewSimpleRenderStringTask(getContent func() string) tasks.TaskFunc {
	return nil
}

func (fake *fakeGuiForSetItems) FocusY(selectedLine int, itemCount int, view *gocui.View) {}

func (fake *fakeGuiForSetItems) ShouldRefresh(contextKey string) bool { return true }

func (fake *fakeGuiForSetItems) GetMainView() *gocui.View { return nil }

func (fake *fakeGuiForSetItems) IsCurrentView(*gocui.View) bool { return false }

func (fake *fakeGuiForSetItems) FilterString(view *gocui.View) string { return "" }

func (fake *fakeGuiForSetItems) IgnoreStrings() []string { return nil }

func (fake *fakeGuiForSetItems) Update(func() error) {}

func (fake *fakeGuiForSetItems) QueueTask(fn func(ctx context.Context)) error { return nil }

type setItem struct {
	id string
}

func TestSetItemsKeepsSelectionOnReusedObject(t *testing.T) {
	a := &setItem{id: "a"}
	b := &setItem{id: "b"}
	c := &setItem{id: "c"}

	panel := &SideListPanel[*setItem]{
		ListPanel: ListPanel[*setItem]{
			List: NewFilteredList[*setItem](),
		},
		Gui: &fakeGuiForSetItems{},
	}

	panel.SetItems([]*setItem{a, b, c})
	panel.SelectedIdx = 1 // select b

	// b is still present (same reused pointer) but at a different index;
	// the cursor must follow it instead of sticking to index 1.
	panel.SetItems([]*setItem{a, c, b})

	assert.Equal(t, 2, panel.SelectedIdx)
	assert.Same(t, b, panel.List.Get(panel.SelectedIdx))
}

func TestSetItemsClampsWhenSelectedObjectDisappears(t *testing.T) {
	a := &setItem{id: "a"}
	b := &setItem{id: "b"}
	c := &setItem{id: "c"}

	panel := &SideListPanel[*setItem]{
		ListPanel: ListPanel[*setItem]{
			List: NewFilteredList[*setItem](),
		},
		Gui: &fakeGuiForSetItems{},
	}

	panel.SetItems([]*setItem{a, b, c})
	panel.SelectedIdx = 1 // select b

	// b has gone: fall back to a valid index rather than tracking a stale row
	panel.SetItems([]*setItem{a, c})

	assert.Equal(t, 1, panel.SelectedIdx)
	assert.Same(t, c, panel.List.Get(panel.SelectedIdx))
}

func TestSetItemsFirstLoadStartsAtZero(t *testing.T) {
	a := &setItem{id: "a"}

	panel := &SideListPanel[*setItem]{
		ListPanel: ListPanel[*setItem]{
			List: NewFilteredList[*setItem](),
		},
		Gui: &fakeGuiForSetItems{},
	}

	panel.SetItems([]*setItem{a})

	assert.Equal(t, 0, panel.SelectedIdx)
}
