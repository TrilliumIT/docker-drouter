package drouter

import (
	"fmt"
	"testing"

	log "github.com/Sirupsen/logrus"
	logtest "github.com/Sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dockerTypes "github.com/docker/engine-api/types"
	dockerNTypes "github.com/docker/engine-api/types/network"
)

const (
	NET_NAME  = "drntest_n%v"
	NET_GW    = "192.168.242.%v"
	NET_IPNET = "192.168.242.%v/29"
	DR_INST   = "dr_test"
)

func createNetwork(n int, dr bool, t *testing.T) string {
	name := fmt.Sprintf(NET_NAME, n)
	opts := make(map[string]string)
	if dr {
		opts["drouter"] = DR_INST
	}
	r, err := dc.NetworkCreate(bg, name, dockerTypes.NetworkCreate{
		Options: opts,
		IPAM: dockerNTypes.IPAM{
			Config: []dockerNTypes.IPAMConfig{
				dockerNTypes.IPAMConfig{
					Subnet:  fmt.Sprintf(NET_IPNET, n*8),
					Gateway: fmt.Sprintf(NET_GW, n*8+1),
				},
			},
		},
	})
	require.Equal(t, err, nil, "Error creating network")
	return r.ID
}

func removeNetwork(id string, t *testing.T) {
	err := dc.NetworkRemove(bg, id)
	if err != nil {
		t.Fatalf("Error removing network: %v", err)
	}
}

func TestNetworkConnect(t *testing.T) {
	assert := assert.New(t)
	ipOffset = 0
	dockerClient = dc

	n0ID := createNetwork(0, true, t)
	n0r, err := dc.NetworkInspect(bg, n0ID)
	assert.Equal(err, nil, "Error inspecting network")

	hook := logtest.NewGlobal()
	n0 := newNetwork(&n0r)
	n0.connect()
	for _, e := range hook.Entries {
		assert.Equal(log.DebugLevel, e.Level, "All messages should be debug")
	}
	n0.disconnect()
	for _, e := range hook.Entries {
		assert.Equal(log.DebugLevel, e.Level, "All messages should be debug")
	}
}
