package drouter

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dockerTypes "github.com/docker/engine-api/types"
	dockerCTypes "github.com/docker/engine-api/types/container"
	dockerNTypes "github.com/docker/engine-api/types/network"
)

const (
	ContName  = "drntest_c%v"
	ContImage = "alpine"
)

func createContainer(cn int, n string) (*container, error) {
	r, err := dc.ContainerCreate(bg,
		&dockerCTypes.Config{
			Image:      ContImage,
			Entrypoint: []string{"/bin/sleep", "600"},
		},
		&dockerCTypes.HostConfig{},
		&dockerNTypes.NetworkingConfig{
			EndpointsConfig: map[string]*dockerNTypes.EndpointSettings{
				n: {},
			},
		}, fmt.Sprintf(ContName, cn))
	if err != nil {
		return nil, err
	}

	err = dc.ContainerStart(bg, r.ID, dockerTypes.ContainerStartOptions{})
	if err != nil {
		return nil, err
	}

	return newContainerFromID(r.ID)
}

func (c *container) remove() error {
	err := dc.ContainerKill(bg, c.id, "")
	if err != nil && !strings.Contains(err.Error(), "is not running") {
		return err
	}
	return dc.ContainerRemove(bg, c.id, dockerTypes.ContainerRemoveOptions{})
}

func TestInvalidContainer(t *testing.T) {
	require.NoError(t, cleanup(), "Failed to cleanup()")

	assert := assert.New(t)
	_, err := newContainerFromID("gibberish_nonsense")
	assert.NotEqual(err, nil, "Inspect invalid container should fail")
}

func TestNonRunningContainer(t *testing.T) {
	require.NoError(t, cleanup(), "Failed to cleanup()")

	require := require.New(t)

	n0r, err := createNetwork(0, true)
	require.NoError(err, "Failed to create n0.")
	//defer func() { require.NoError(dc.NetworkRemove(bg, n0r.ID), "Failed to remove n0.") }()

	c, err := createContainer(0, n0r.ID)
	require.NoError(err, "Failed to create c0.")
	//defer func() { require.NoError(c.remove(), "Failed to remove c0.") }()

	err = dc.ContainerKill(bg, c.id, "")
	require.NoError(err, "Error stopping container")
	time.Sleep(1 * time.Second)

	cStopped, err := newContainerFromID(c.id)
	require.NoError(err, "Inspect stopped container should succeed")

	require.Nil(cStopped.handle, "Inspect on stopped container should return nil handle")
}
