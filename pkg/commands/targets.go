package commands

import "github.com/jesseduffield/lazydocker/pkg/ledger"

// This file maps domain objects onto ledger targets, so every record carries
// the operated object's type and identifier plus, where known, its compose
// project and service.

// LedgerTarget identifies a container for ledger records.
func (c *Container) LedgerTarget() ledger.Target {
	return ledger.Target{
		Kind:    ledger.ObjectContainer,
		ID:      c.ID,
		Name:    c.Name,
		Project: c.ProjectName,
		Service: c.ServiceName,
	}
}

// LedgerTarget identifies an image for ledger records.
func (i *Image) LedgerTarget() ledger.Target {
	name := i.Name
	if i.Tag != "" {
		name = i.Name + ":" + i.Tag
	}
	return ledger.Target{
		Kind: ledger.ObjectImage,
		ID:   i.ID,
		Name: name,
	}
}

// LedgerTarget identifies a volume for ledger records.
func (v *Volume) LedgerTarget() ledger.Target {
	return ledger.Target{
		Kind: ledger.ObjectVolume,
		ID:   v.Name,
		Name: v.Name,
	}
}

// LedgerTarget identifies a network for ledger records.
func (n *Network) LedgerTarget() ledger.Target {
	return ledger.Target{
		Kind: ledger.ObjectNetwork,
		ID:   n.Name,
		Name: n.Name,
	}
}

// LedgerTarget identifies a compose service for ledger records.
func (s *Service) LedgerTarget() ledger.Target {
	return ledger.Target{
		Kind:    ledger.ObjectService,
		ID:      s.ID,
		Name:    s.Name,
		Project: s.ProjectName,
		Service: s.Name,
	}
}

// ProjectLedgerTarget identifies a compose project for ledger records.
func ProjectLedgerTarget(name string) ledger.Target {
	return ledger.Target{
		Kind: ledger.ObjectProject,
		ID:   name,
		Name: name,
	}
}

// CommandObjectLedgerTarget derives the ledger target from a custom-command
// context. It prefers the most specific object available.
func CommandObjectLedgerTarget(obj CommandObject) ledger.Target {
	switch {
	case obj.Container != nil:
		return obj.Container.LedgerTarget()
	case obj.Service != nil:
		return obj.Service.LedgerTarget()
	case obj.Image != nil:
		return obj.Image.LedgerTarget()
	case obj.Volume != nil:
		return obj.Volume.LedgerTarget()
	case obj.Network != nil:
		return obj.Network.LedgerTarget()
	case obj.Project != nil:
		return ProjectLedgerTarget(obj.Project.Name)
	default:
		return ledger.Target{}
	}
}
