package commands

import (
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/stretchr/testify/assert"
)

func imageSummaryWithTag(tag string) image.Summary {
	return image.Summary{RepoTags: []string{tag}}
}

func volType(name string) *volume.Volume {
	return &volume.Volume{Name: name}
}

func networkInspect(name string) network.Inspect {
	return network.Inspect{Name: name}
}

func containerRef(c *Container) ObjectRef {
	return ObjectRef{Type: ContainerObject, ID: c.ID, Name: c.Name}
}

func fullContainer(id, name, imageID, imageName, project, service string) *Container {
	return &Container{
		ID:   id,
		Name: name,
		Container: container.Summary{
			ImageID: imageID,
			Image:   imageName,
			Mounts: []container.MountPoint{
				{Type: mount.TypeVolume, Name: "vol1", Destination: "/data"},
			},
			NetworkSettings: &container.NetworkSettingsSummary{
				Networks: map[string]*network.EndpointSettings{
					"my-net": {IPAddress: "172.17.0.2"},
				},
			},
		},
		Details: container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{Image: imageID},
			Mounts: []container.MountPoint{
				{Type: mount.TypeVolume, Name: "vol1", Destination: "/data"},
			},
			Config: &container.Config{Image: imageName},
			NetworkSettings: &container.NetworkSettings{
				Networks: map[string]*network.EndpointSettings{
					"my-net": {IPAddress: "172.17.0.2"},
				},
			},
		},
		ProjectName: project,
		ServiceName: service,
	}
}

func TestBuildRelationGraphForwardAndReverse(t *testing.T) {
	ctr := fullContainer("sha256:c1", "web", "sha256:img1", "nginx:latest", "proj1", "web")

	img := &Image{ID: "sha256:img1", Image: imageSummaryWithTag("nginx:latest")}
	vol := &Volume{Name: "vol1", Volume: volType("vol1")}
	nw := &Network{Name: "my-net", Network: networkInspect("my-net")}
	svc := &Service{Name: "web", ProjectName: "proj1"}

	graph := BuildRelationGraph([]*Container{ctr}, []*Image{img}, []*Volume{vol}, []*Network{nw}, []*Service{svc})

	assert.True(t, graph.Ready())

	// forward: container -> image/volume/network
	out := graph.References(containerRef(ctr))
	assert.Len(t, out, 3)
	kinds := map[ReferenceKind]bool{}
	for _, e := range out {
		kinds[e.Kind] = true
	}
	assert.True(t, kinds[ImageReference])
	assert.True(t, kinds[VolumeReference])
	assert.True(t, kinds[NetworkReference])

	// reverse: image -> container, attributed to project and service
	imageNode := ObjectRef{Type: ImageObject, ID: "sha256:img1"}
	refs := graph.Referrers(imageNode)
	assert.Len(t, refs, 1)
	assert.Equal(t, "proj1", refs[0].Project)
	assert.Equal(t, "web", refs[0].Service)
	assert.Equal(t, "sha256:c1", refs[0].Referrer.ID)

	// reverse: volume and network
	assert.Len(t, graph.Referrers(ObjectRef{Type: VolumeObject, ID: "vol1"}), 1)
	assert.Len(t, graph.Referrers(ObjectRef{Type: NetworkObject, ID: "my-net"}), 1)

	// forward from a service and a project: union of their containers
	serviceEdges := graph.References(ObjectRef{Type: ServiceObject, ID: "proj1-web"})
	assert.Len(t, serviceEdges, 3)
	projectEdges := graph.References(ObjectRef{Type: ProjectObject, ID: "proj1"})
	assert.Len(t, projectEdges, 3)

	// every observed object is a node
	assert.True(t, graph.HasNode(imageNode))
	assert.True(t, graph.HasNode(ObjectRef{Type: VolumeObject, ID: "vol1"}))
	assert.True(t, graph.HasNode(ObjectRef{Type: NetworkObject, ID: "my-net"}))
	assert.True(t, graph.HasNode(ObjectRef{Type: ProjectObject, ID: "proj1"}))
}

func TestImageIDContradictionIsUnknown(t *testing.T) {
	ctr := fullContainer("sha256:c1", "web", "sha256:img1", "nginx:latest", "proj1", "web")
	// inspect disagrees with the list
	ctr.Details.Image = "sha256:img2"

	img1 := &Image{ID: "sha256:img1"}
	img2 := &Image{ID: "sha256:img2"}
	graph := BuildRelationGraph([]*Container{ctr}, []*Image{img1, img2}, nil, nil, nil)

	assert.True(t, graph.IsUnknown(containerRef(ctr), ImageReference))
	// no contradictory edges may be published for either image
	assert.Empty(t, graph.Referrers(ObjectRef{Type: ImageObject, ID: "sha256:img1"}))
	assert.Empty(t, graph.Referrers(ObjectRef{Type: ImageObject, ID: "sha256:img2"}))

	unknowns := graph.Uncertainties(ImageReference)
	assert.NotEmpty(t, unknowns)
}

func TestVolumeSetContradictionIsUnknown(t *testing.T) {
	ctr := fullContainer("sha256:c1", "web", "sha256:img1", "nginx:latest", "proj1", "web")
	// inspect sees an extra volume
	ctr.Details.Mounts = append(ctr.Details.Mounts, container.MountPoint{
		Type: mount.TypeVolume, Name: "vol2", Destination: "/other",
	})

	graph := BuildRelationGraph([]*Container{ctr}, nil, nil, nil, nil)

	assert.True(t, graph.IsUnknown(containerRef(ctr), VolumeReference))
	assert.Empty(t, graph.Referrers(ObjectRef{Type: VolumeObject, ID: "vol1"}))
	assert.Empty(t, graph.Referrers(ObjectRef{Type: VolumeObject, ID: "vol2"}))
}

func TestNetworkSourcesMissingIsUnknown(t *testing.T) {
	ctr := fullContainer("sha256:c1", "web", "sha256:img1", "nginx:latest", "proj1", "web")
	// both sources of network membership are gone
	ctr.Container.NetworkSettings = nil
	ctr.Details.NetworkSettings = nil

	graph := BuildRelationGraph([]*Container{ctr}, nil, nil, nil, nil)

	assert.True(t, graph.IsUnknown(containerRef(ctr), NetworkReference))
	// unknown is not the same as "connected to nothing": there must be no
	// edge claiming an empty set
	assert.Empty(t, graph.Referrers(ObjectRef{Type: NetworkObject, ID: "my-net"}))
}

func TestListOnlyWhenDetailsMissing(t *testing.T) {
	ctr := fullContainer("sha256:c1", "web", "sha256:img1", "nginx:latest", "proj1", "web")
	// simulate an inspect failure: no details at all
	ctr.Details = container.InspectResponse{}

	graph := BuildRelationGraph([]*Container{ctr}, nil, nil, nil, nil)

	out := graph.References(containerRef(ctr))
	assert.Len(t, out, 3)
	assert.False(t, graph.IsUnknown(containerRef(ctr), ImageReference))
	assert.False(t, graph.IsUnknown(containerRef(ctr), VolumeReference))
	assert.False(t, graph.IsUnknown(containerRef(ctr), NetworkReference))
}

func TestEmptyMountSetsAgree(t *testing.T) {
	ctr := fullContainer("sha256:c1", "web", "sha256:img1", "nginx:latest", "", "")
	ctr.Container.Mounts = nil
	ctr.Details.Mounts = nil

	graph := BuildRelationGraph([]*Container{ctr}, nil, nil, nil, nil)

	assert.False(t, graph.IsUnknown(containerRef(ctr), VolumeReference))

	// two agreeing empty observations mean "no volume mounts" — a definite
	// statement; image and network edges remain, volume edges do not.
	kinds := map[ReferenceKind]int{}
	for _, e := range graph.References(containerRef(ctr)) {
		kinds[e.Kind]++
	}
	assert.Equal(t, 0, kinds[VolumeReference])
	assert.Equal(t, 1, kinds[ImageReference])
	assert.Equal(t, 1, kinds[NetworkReference])
}

func TestImageContainerCountMismatch(t *testing.T) {
	ctr := fullContainer("sha256:c1", "web", "sha256:img1", "nginx:latest", "proj1", "web")
	img := &Image{ID: "sha256:img1"}
	img.Image.Containers = 5 // daemon says 5, graph will show 1

	graph := BuildRelationGraph([]*Container{ctr}, []*Image{img}, nil, nil, nil)

	assert.True(t, graph.IsUnknown(ObjectRef{Type: ImageObject, ID: "sha256:img1"}, ImageReference))
}

func TestOneOffNotAttributedToService(t *testing.T) {
	ctr := fullContainer("sha256:c1", "web_run", "sha256:img1", "nginx:latest", "proj1", "web")
	ctr.OneOff = true

	graph := BuildRelationGraph([]*Container{ctr}, nil, nil, nil, nil)

	refs := graph.Referrers(ObjectRef{Type: ImageObject, ID: "sha256:img1"})
	assert.Len(t, refs, 1)
	assert.Equal(t, "proj1", refs[0].Project)
	assert.Equal(t, "", refs[0].Service)
}

func TestAtomicSnapshotSwap(t *testing.T) {
	c := &DockerCommand{}
	assert.Nil(t, c.Relations())

	ctr := fullContainer("sha256:c1", "web", "sha256:img1", "nginx:latest", "proj1", "web")
	c.RefreshRelations([]*Container{ctr}, nil, nil, nil, nil)
	first := c.Relations()
	assert.True(t, first.Ready())

	ctr2 := fullContainer("sha256:c2", "db", "sha256:img2", "redis:latest", "proj1", "db")
	c.RefreshRelations([]*Container{ctr, ctr2}, nil, nil, nil, nil)
	second := c.Relations()
	assert.True(t, second.Ready())

	// the old snapshot remains intact and usable — never half updated
	assert.Len(t, first.References(containerRef(ctr)), 3)
	assert.Len(t, second.References(containerRef(ctr2)), 3)
}
