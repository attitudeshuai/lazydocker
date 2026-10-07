package gui

import (
	"github.com/jesseduffield/lazydocker/pkg/commands"
	"github.com/jesseduffield/lazydocker/pkg/gui/presentation"
	"github.com/jesseduffield/lazydocker/pkg/tasks"
)

// renderContainerUses renders the forward lookup of a container: its image,
// mounted volumes and connected networks.
func (gui *Gui) renderContainerUses(ctr *commands.Container) tasks.TaskFunc {
	return gui.NewSimpleRenderStringTask(func() string {
		return presentation.RenderReferences(
			gui.DockerCommand.Relations(),
			commands.ObjectRef{Type: commands.ContainerObject, ID: ctr.ID},
		)
	})
}

// renderImageUsedBy renders the reverse lookup of an image: the containers
// using it, attributed to project and service.
func (gui *Gui) renderImageUsedBy(img *commands.Image) tasks.TaskFunc {
	return gui.NewSimpleRenderStringTask(func() string {
		return presentation.RenderReferrers(
			gui.DockerCommand.Relations(),
			commands.ObjectRef{Type: commands.ImageObject, ID: img.ID},
		)
	})
}

// renderVolumeUsedBy renders the reverse lookup of a volume.
func (gui *Gui) renderVolumeUsedBy(volume *commands.Volume) tasks.TaskFunc {
	return gui.NewSimpleRenderStringTask(func() string {
		return presentation.RenderReferrers(
			gui.DockerCommand.Relations(),
			commands.ObjectRef{Type: commands.VolumeObject, ID: volume.Name},
		)
	})
}

// renderNetworkUsedBy renders the reverse lookup of a network.
func (gui *Gui) renderNetworkUsedBy(network *commands.Network) tasks.TaskFunc {
	return gui.NewSimpleRenderStringTask(func() string {
		return presentation.RenderReferrers(
			gui.DockerCommand.Relations(),
			commands.ObjectRef{Type: commands.NetworkObject, ID: network.Name},
		)
	})
}
