package commands

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetServicesFromContainersReusesExistingObjects(t *testing.T) {
	c := &DockerCommand{}

	existing := &Service{ID: "proj-web", Name: "web", ProjectName: "proj"}
	containers := []*Container{
		{ID: "c1", ServiceName: "web", ProjectName: "proj"},
	}

	got := c.GetServicesFromContainers(containers, []*Service{existing})

	if assert.Len(t, got, 1) {
		// same pointer: the panel can keep selection without rebuilding
		assert.Same(t, existing, got[0])
		assert.Equal(t, "web", got[0].Name)
		assert.Equal(t, "proj", got[0].ProjectName)
	}
}

func TestGetServicesFromContainersSkipsOneOffsAndDuplicates(t *testing.T) {
	c := &DockerCommand{}

	containers := []*Container{
		{ID: "c1", ServiceName: "web", ProjectName: "proj", OneOff: true},
		{ID: "c2", ServiceName: "db", ProjectName: "proj"},
		{ID: "c3", ServiceName: "db", ProjectName: "proj"},
		{ID: "c4", ServiceName: "worker"},
	}

	got := c.GetServicesFromContainers(containers, nil)

	assert.Len(t, got, 2)
	assert.Equal(t, "proj-db", got[0].ID)
	assert.Equal(t, "-worker", got[1].ID)
}
