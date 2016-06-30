package drouter

import (
	"fmt"
	"testing"
	"time"

	//log "github.com/Sirupsen/logrus"
	logtest "github.com/Sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dockerTypes "github.com/docker/engine-api/types"
	dockerCTypes "github.com/docker/engine-api/types/container"
	dockerNTypes "github.com/docker/engine-api/types/network"
)

const (
	CONT_NAME  = "drntest_c%v"
	CONT_IMAGE = "alpine"
)

func createContainer(cn, n string, t *testing.T) string {
	r, err := dc.ContainerCreate(bg,
		&dockerCTypes.Config{
			Image:      CONT_IMAGE,
			Entrypoint: []string{"/bin/sleep", "600"},
		},
		&dockerCTypes.HostConfig{},
		&dockerNTypes.NetworkingConfig{
			EndpointsConfig: map[string]*dockerNTypes.EndpointSettings{
				n: &dockerNTypes.EndpointSettings{},
			},
		}, fmt.Sprintf(CONT_NAME, cn))
	require.Equal(t, err, nil, "Error creating container")

	err = dc.ContainerStart(bg, r.ID, dockerTypes.ContainerStartOptions{})

	assert.Equal(t, err, nil, "Error starting container")
	if err != nil {
		removeContainer(r.ID, t)
	}

	return r.ID
}

func removeContainer(id string, t *testing.T) {
	dc.ContainerKill(bg, id, "")
	err := dc.ContainerRemove(bg, id, dockerTypes.ContainerRemoveOptions{})
	require.Equal(t, err, nil, "Error removing container")
}

func TestNewContainer(t *testing.T) {
	assert := assert.New(t)

	n0r := createNetwork(0, true, t)
	defer removeNetwork(n0r.ID, t)

	cid := createContainer("0", n0r.Name, t)
	defer removeContainer(cid, t)

	hook := logtest.NewGlobal()
	defer hook.Reset()
	_, err := newContainerFromID(cid)
	assert.Equal(err, nil, "Failed to get container object")
	checkLogs(hook.Entries, t)
}

func TestInvalidContainer(t *testing.T) {
	assert := assert.New(t)
	_, err := newContainerFromID("gibberish_nonsense")
	assert.NotEqual(err, nil, "Inspect invalid container should fail")
}

func TestNonRunningContainer(t *testing.T) {
	assert := assert.New(t)

	n0r := createNetwork(0, true, t)
	defer removeNetwork(n0r.ID, t)

	cid := createContainer("0", n0r.Name, t)
	defer removeContainer(cid, t)

	err := dc.ContainerKill(bg, cid, "")
	assert.Equal(err, nil, "Error stopping container")
	time.Sleep(1 * time.Second)

	c, err := newContainerFromID(cid)
	assert.Equal(err, nil, "Inspect stopped container should succeed")

	assert.Nil(c.handle, "Inspect on stopped container should return nil handle")
}
