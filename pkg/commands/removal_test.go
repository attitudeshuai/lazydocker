package commands

import (
	"errors"
	"testing"

	"github.com/jesseduffield/lazydocker/pkg/config"
	"github.com/stretchr/testify/assert"
)

func newTestCommand(cfg *config.UserConfig) *DockerCommand {
	return &DockerCommand{
		Config: &config.AppConfig{UserConfig: cfg},
	}
}

func TestNoPolicyMeansLegacyAndUnchangedRemoval(t *testing.T) {
	c := newTestCommand(&config.UserConfig{})

	target := ObjectRef{Type: ImageObject, ID: "sha256:img1"}
	plan := c.PlanDelete(target, "")
	assert.Equal(t, PolicyLegacy, plan.Action)

	called := false
	err := c.ExecuteDelete(plan, func(ref ObjectRef) error {
		called = true
		assert.Equal(t, target, ref)
		return nil
	})
	assert.NoError(t, err)
	assert.True(t, called)
}

func TestRejectBlocksAndListsReferrers(t *testing.T) {
	cfg := &config.UserConfig{
		DeletePolicy: config.DeletePolicyConfig{
			ByType: map[string]string{"image": "reject"},
		},
	}
	c := newTestCommand(cfg)

	ctr := fullContainer("sha256:c1", "web", "sha256:img1", "nginx:latest", "proj1", "web")
	c.RefreshRelations([]*Container{ctr}, nil, nil, nil, nil)

	target := ObjectRef{Type: ImageObject, ID: "sha256:img1"}
	plan := c.PlanDelete(target, "")
	assert.Equal(t, PolicyReject, plan.Action)
	assert.True(t, plan.Blocked())
	assert.Len(t, plan.Referrers, 1)

	// executing a rejected plan must fail before any request is sent
	err := c.ExecuteDelete(plan, func(ref ObjectRef) error {
		t.Fatal("remove must not be called on a rejected plan")
		return nil
	})
	var blockErr *ReferenceBlockError
	assert.True(t, errors.As(err, &blockErr))
	assert.Len(t, blockErr.Referrers, 1)
}

func TestWarnPlanConfirmedRemoves(t *testing.T) {
	cfg := &config.UserConfig{
		DeletePolicy: config.DeletePolicyConfig{
			ByType: map[string]string{"image": "warn"},
		},
	}
	c := newTestCommand(cfg)

	ctr := fullContainer("sha256:c1", "web", "sha256:img1", "nginx:latest", "proj1", "web")
	c.RefreshRelations([]*Container{ctr}, nil, nil, nil, nil)

	plan := c.PlanDelete(ObjectRef{Type: ImageObject, ID: "sha256:img1"}, "")
	assert.Equal(t, PolicyWarn, plan.Action)
	assert.True(t, plan.Blocked())

	// GUI confirmation is what gates warn; after confirmation the engine
	// performs the requested removal exactly.
	called := false
	err := c.ExecuteDelete(plan, func(ref ObjectRef) error {
		called = true
		return nil
	})
	assert.NoError(t, err)
	assert.True(t, called)
}

func TestCascadeRemovesLayerByLayer(t *testing.T) {
	cfg := &config.UserConfig{
		DeletePolicy: config.DeletePolicyConfig{
			ByType: map[string]string{"image": "cascade"},
		},
	}
	c := newTestCommand(cfg)

	ctr := fullContainer("sha256:c1", "web", "sha256:img1", "nginx:latest", "proj1", "web")
	c.RefreshRelations([]*Container{ctr}, nil, nil, nil, nil)

	target := ObjectRef{Type: ImageObject, ID: "sha256:img1"}
	plan := c.PlanDelete(target, "")
	assert.Equal(t, PolicyCascade, plan.Action)
	assert.Len(t, plan.Layers, 2) // target + one referrer layer

	var order []string
	err := c.ExecuteDelete(plan, func(ref ObjectRef) error {
		order = append(order, ref.ID)
		return nil
	})
	assert.NoError(t, err)
	// referrer removed first, then target
	assert.Equal(t, []string{"sha256:c1", "sha256:img1"}, order)
}

func TestCascadeBlocksWhenGraphNotReady(t *testing.T) {
	cfg := &config.UserConfig{
		DeletePolicy: config.DeletePolicyConfig{Default: "cascade"},
	}
	c := newTestCommand(cfg)

	// no RefreshRelations: references are unknown, not absent
	plan := c.PlanDelete(ObjectRef{Type: VolumeObject, ID: "vol1"}, "")
	assert.Equal(t, PolicyCascade, plan.Action)
	assert.True(t, plan.Blocked())

	err := c.ExecuteDelete(plan, func(ref ObjectRef) error {
		t.Fatal("remove must not run while cascade references are unknown")
		return nil
	})
	var blockErr *ReferenceBlockError
	assert.True(t, errors.As(err, &blockErr))
}

func TestBatchLegacyContinuesPastErrors(t *testing.T) {
	c := newTestCommand(&config.UserConfig{})

	target1 := ObjectRef{Type: ContainerObject, ID: "sha256:c1"}
	target2 := ObjectRef{Type: ContainerObject, ID: "sha256:c2"}
	plans := c.PlanBatch([]BatchTarget{{Target: target1}, {Target: target2}})
	assert.True(t, AllPlansLegacy(plans))

	report := c.ExecuteBatch(plans, func(ref ObjectRef) error {
		if ref.ID == "sha256:c1" {
			return errors.New("boom")
		}
		return nil
	})

	assert.Equal(t, 1, report.Removed)
	assert.Len(t, report.Failed, 1)
	assert.Empty(t, report.Skipped)
	assert.True(t, report.HasIssues())
}

func TestBatchMixedSkipsRejectedAndRemovesOthers(t *testing.T) {
	cfg := &config.UserConfig{
		DeletePolicy: config.DeletePolicyConfig{
			ByType: map[string]string{"image": "reject"},
		},
	}
	c := newTestCommand(cfg)

	// image referenced by a container: skipped; standalone container: removed
	ctr := fullContainer("sha256:c1", "web", "sha256:img1", "nginx:latest", "proj1", "web")
	c.RefreshRelations([]*Container{ctr}, nil, nil, nil, nil)

	plans := c.PlanBatch([]BatchTarget{
		{Target: ObjectRef{Type: ImageObject, ID: "sha256:img1"}},
		{Target: ObjectRef{Type: ContainerObject, ID: "sha256:c1"}, Project: "proj1"},
	})

	report := c.ExecuteBatch(plans, func(ref ObjectRef) error {
		return nil
	})

	assert.Len(t, report.Skipped, 1)
	assert.Equal(t, ImageObject, report.Skipped[0].Target.Type)
	assert.Equal(t, 1, report.Removed)
}

func TestPolicyResolutionPrecedence(t *testing.T) {
	image := ImageObject

	t.Run("default", func(t *testing.T) {
		p := config.DeletePolicyConfig{Default: "reject"}
		assert.Equal(t, PolicyReject, resolvePolicy(p, image, ""))
	})

	t.Run("by type", func(t *testing.T) {
		p := config.DeletePolicyConfig{ByType: map[string]string{"image": "warn"}}
		assert.Equal(t, PolicyWarn, resolvePolicy(p, image, "proj1"))
	})

	t.Run("project overrides type", func(t *testing.T) {
		p := config.DeletePolicyConfig{
			ByType:    map[string]string{"image": "reject"},
			ByProject: map[string]string{"proj1": "warn"},
		}
		assert.Equal(t, PolicyWarn, resolvePolicy(p, image, "proj1"))
		assert.Equal(t, PolicyReject, resolvePolicy(p, image, "other"))
	})

	t.Run("specific rule beats maps", func(t *testing.T) {
		p := config.DeletePolicyConfig{
			ByType:    map[string]string{"image": "reject"},
			ByProject: map[string]string{"proj1": "warn"},
			Rules: []config.DeletePolicyRule{
				{ObjectType: "image", Project: "proj1", Action: "cascade"},
			},
		}
		assert.Equal(t, PolicyCascade, resolvePolicy(p, image, "proj1"))
	})

	t.Run("single dimension rule", func(t *testing.T) {
		p := config.DeletePolicyConfig{
			ByType: map[string]string{"image": "reject"},
			Rules: []config.DeletePolicyRule{
				{Project: "proj1", Action: "warn"},
			},
		}
		assert.Equal(t, PolicyWarn, resolvePolicy(p, image, "proj1"))
		assert.Equal(t, PolicyReject, resolvePolicy(p, image, "other"))
	})

	t.Run("plural type names accepted", func(t *testing.T) {
		p := config.DeletePolicyConfig{
			Rules: []config.DeletePolicyRule{
				{ObjectType: "images", Project: "proj1", Action: "reject"},
			},
		}
		assert.Equal(t, PolicyReject, resolvePolicy(p, image, "proj1"))
	})

	t.Run("unknown action falls back to legacy", func(t *testing.T) {
		c := newTestCommand(&config.UserConfig{
			DeletePolicy: config.DeletePolicyConfig{Default: "bogus"},
		})
		assert.Equal(t, PolicyLegacy, c.resolveDeleteAction(image, ""))
	})
}
