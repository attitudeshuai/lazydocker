package commands

import (
	"errors"
	"testing"

	"github.com/jesseduffield/lazydocker/pkg/ledger"
	"github.com/stretchr/testify/assert"
)

func TestCommandObjectLedgerTargetPicksMostSpecificObject(t *testing.T) {
	ctr := &Container{ID: "cid", Name: "cname", ProjectName: "proj", ServiceName: "svc"}
	svc := &Service{ID: "proj-svc", Name: "svc", ProjectName: "proj"}
	img := &Image{ID: "imgid", Name: "repo", Tag: "latest"}
	vol := &Volume{Name: "volname"}
	nw := &Network{Name: "nwname"}
	proj := &Project{Name: "projname"}

	assert.Equal(t, ledger.Target{Kind: ledger.ObjectContainer, ID: "cid", Name: "cname", Project: "proj", Service: "svc"},
		CommandObjectLedgerTarget(CommandObject{Container: ctr, Service: svc}))
	assert.Equal(t, ledger.Target{Kind: ledger.ObjectService, ID: "proj-svc", Name: "svc", Project: "proj", Service: "svc"},
		CommandObjectLedgerTarget(CommandObject{Service: svc}))
	assert.Equal(t, ledger.Target{Kind: ledger.ObjectImage, ID: "imgid", Name: "repo:latest"},
		CommandObjectLedgerTarget(CommandObject{Image: img}))
	assert.Equal(t, ledger.Target{Kind: ledger.ObjectVolume, ID: "volname", Name: "volname"},
		CommandObjectLedgerTarget(CommandObject{Volume: vol}))
	assert.Equal(t, ledger.Target{Kind: ledger.ObjectNetwork, ID: "nwname", Name: "nwname"},
		CommandObjectLedgerTarget(CommandObject{Network: nw}))
	assert.Equal(t, ledger.Target{Kind: ledger.ObjectProject, ID: "projname", Name: "projname"},
		CommandObjectLedgerTarget(CommandObject{Project: proj}))
	assert.Equal(t, ledger.Target{}, CommandObjectLedgerTarget(CommandObject{}))
}

func TestDisabledLedgerChainIsNoOp(t *testing.T) {
	// A nil ledger (test dummies) and an explicitly disabled ledger must both
	// be inert no-ops on the recording path.
	var nilBook *ledger.Ledger
	err := nilBook.Start(ledger.PathAPI, "container.ping").
		For(ledger.Target{Kind: ledger.ObjectContainer, ID: "x"}).
		Run(func() error { return nil })
	assert.NoError(t, err)

	disabledBook, err := ledger.New(ledger.Config{Enabled: false, Dir: t.TempDir()})
	assert.NoError(t, err)
	defer disabledBook.Close()
	assert.False(t, disabledBook.Enabled())
	assert.NoError(t, disabledBook.Start(ledger.PathProcess, "custom-command").
		FinishProcess("docker stop x", 7, nil))
	// a failing command keeps exactly its original error when disabled
	cmdErr := errors.New("exit status 1")
	assert.Equal(t, cmdErr, disabledBook.Start(ledger.PathProcess, "custom-command").
		FinishProcess("docker stop x", 1, cmdErr))
	// the persistence-only outlet reports nothing at all when disabled
	assert.NoError(t, disabledBook.Start(ledger.PathProcess, "attach").
		RecordProcessResult("docker attach x", 130, cmdErr))
	assert.Nil(t, disabledBook.Query(ledger.Filter{}))
}
