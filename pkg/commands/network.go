package commands

import (
	"context"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/jesseduffield/lazydocker/pkg/ledger"
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

// RefreshNetworks gets the networks and stores them
func (c *DockerCommand) RefreshNetworks() ([]*Network, error) {
	networks, err := c.Client.NetworkList(context.Background(), network.ListOptions{})
	if err != nil {
		return nil, err
	}

	ownNetworks := make([]*Network, len(networks))

	for i, nw := range networks {
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
	return c.pruneNetworks("", 0, 0)
}

// BatchPruneNetworks prunes networks, recording the prune as one item of a
// bulk operation.
func (c *DockerCommand) BatchPruneNetworks(batchID string, batchIndex, batchTotal int) error {
	return c.pruneNetworks(batchID, batchIndex, batchTotal)
}

func (c *DockerCommand) pruneNetworks(batchID string, batchIndex, batchTotal int) error {
	return c.TrackAPI("network.prune", ledger.Target{Kind: ledger.ObjectNetwork}, batchID, batchIndex, batchTotal, func() error {
		_, err := c.Client.NetworksPrune(context.Background(), filters.Args{})
		return err
	})
}

// Remove removes the network
func (v *Network) Remove() error {
	return v.DockerCommand.Ledger().Start(ledger.PathAPI, "network.remove").
		For(v.LedgerTarget()).
		Run(func() error {
			return v.Client.NetworkRemove(context.Background(), v.Name)
		})
}
