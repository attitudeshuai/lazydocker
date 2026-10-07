package commands

import (
	"context"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

// Volume : A docker Volume
type Volume struct {
	Name          string
	Volume        *volume.Volume
	Client        *client.Client
	OSCommand     *OSCommand
	Log           *logrus.Entry
	DockerCommand LimitedDockerCommand
}

// RefreshVolumes gets the volumes and stores them. Volumes that were already
// present in existingVolumes are reused (same pointer, refreshed details) so
// that panels can keep selection and avoid rebuilding the whole list.
func (c *DockerCommand) RefreshVolumes(existingVolumes []*Volume) ([]*Volume, error) {
	result, err := c.Client.VolumeList(context.Background(), volume.ListOptions{})
	if err != nil {
		return nil, err
	}

	volumes := result.Volumes

	existingByName := make(map[string]*Volume, len(existingVolumes))
	for _, vol := range existingVolumes {
		existingByName[vol.Name] = vol
	}

	ownVolumes := make([]*Volume, len(volumes))

	for i, vol := range volumes {
		if existingVolume, ok := existingByName[vol.Name]; ok {
			existingVolume.Volume = vol
			ownVolumes[i] = existingVolume
			continue
		}

		ownVolumes[i] = &Volume{
			Name:          vol.Name,
			Volume:        vol,
			Client:        c.Client,
			OSCommand:     c.OSCommand,
			Log:           c.Log,
			DockerCommand: c,
		}
	}

	return ownVolumes, nil
}

// PruneVolumes prunes volumes
func (c *DockerCommand) PruneVolumes() error {
	_, err := c.Client.VolumesPrune(context.Background(), filters.Args{})
	return err
}

// Remove removes the volume
func (v *Volume) Remove(force bool) error {
	return v.Client.VolumeRemove(context.Background(), v.Name, force)
}
