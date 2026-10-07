package commands

import (
	"context"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

// Network : A docker Network
type Network struct {
	Name          string
	Network       network.Inspect
	Client        *client.Client
	OSCommand     *OSCommand
	Log           *logrus.Entry
	DockerCommand LimitedDockerCommand
}

// RefreshNetworks gets the networks and stores them. Networks that were
// already present in existingNetworks are reused (same pointer, refreshed
// details) so that panels can keep selection and avoid rebuilding the whole
// list.
func (c *DockerCommand) RefreshNetworks(existingNetworks []*Network) ([]*Network, error) {
	networks, err := c.Client.NetworkList(context.Background(), network.ListOptions{})
	if err != nil {
		return nil, err
	}

	existingByID := make(map[string]*Network, len(existingNetworks))
	for _, nw := range existingNetworks {
		existingByID[nw.Network.ID] = nw
	}

	ownNetworks := make([]*Network, len(networks))

	for i, nw := range networks {
		if existingNetwork, ok := existingByID[nw.ID]; ok {
			existingNetwork.Network = nw
			existingNetwork.Name = nw.Name
			ownNetworks[i] = existingNetwork
			continue
		}

		ownNetworks[i] = &Network{
			Name:          nw.Name,
			Network:       nw,
			Client:        c.Client,
			OSCommand:     c.OSCommand,
			Log:           c.Log,
			DockerCommand: c,
		}
	}

	return ownNetworks, nil
}

// PruneNetworks prunes networks
func (c *DockerCommand) PruneNetworks() error {
	_, err := c.Client.NetworksPrune(context.Background(), filters.Args{})
	return err
}

// Remove removes the network
func (v *Network) Remove() error {
	return v.Client.NetworkRemove(context.Background(), v.Name)
}
