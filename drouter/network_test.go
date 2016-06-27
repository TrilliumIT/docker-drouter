package drouter

import (
	"fmt"
	"net"
	"testing"

	log "github.com/Sirupsen/logrus"
	logtest "github.com/Sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dockerTypes "github.com/docker/engine-api/types"
	dockerNTypes "github.com/docker/engine-api/types/network"
	"github.com/vishvananda/netlink"
)

const (
	NET_NAME  = "drntest_n%v"
	NET_GW    = "192.168.242.%v"
	NET_IPNET = "192.168.242.%v/29"
)

func createNetwork(n int, dr bool, t *testing.T) dockerTypes.NetworkResource {
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

	nr, err := dc.NetworkInspect(bg, r.ID)
	require.Equal(t, err, nil, "Error inspecting network")
	return nr
}

func removeNetwork(id string, t *testing.T) {
	err := dc.NetworkRemove(bg, id)
	if err != nil {
		t.Fatalf("Error removing network: %v", err)
	}
}

func TestNetworkConnect(t *testing.T) {
	assert := assert.New(t)

	n0r := createNetwork(0, true, t)
	defer removeNetwork(n0r.ID, t)

	hook := logtest.NewGlobal()
	n0 := newNetwork(&n0r)
	n0.connect()
	for _, e := range hook.Entries {
		assert.Equal(log.DebugLevel, e.Level, "All messages should be debug")
	}
	n0.disconnect()
	checkLogs(hook.Entries, t)
}

func TestIPOffset(t *testing.T) {
	assert := assert.New(t)
	ipOffset = 2
	defer func() { ipOffset = 0 }()

	n0r := createNetwork(0, true, t)
	defer removeNetwork(n0r.ID, t)

	hook := logtest.NewGlobal()
	n0 := newNetwork(&n0r)
	n0.connect()

	routes, err := netlink.RouteGet(net.ParseIP("192.168.242.1"))
	assert.Equal(err, nil, "Error getting routes")
	assert.True(routes[0].Src.Equal(net.ParseIP("192.168.242.2")), "IP not what was expected")

	n0.disconnect()
	checkLogs(hook.Entries, t)
}

func TestNegativeIPOffset(t *testing.T) {
	assert := assert.New(t)
	ipOffset = -1
	defer func() { ipOffset = 0 }()

	n0r := createNetwork(0, true, t)
	defer removeNetwork(n0r.ID, t)

	hook := logtest.NewGlobal()
	n0 := newNetwork(&n0r)
	n0.connect()

	routes, err := netlink.RouteGet(net.ParseIP("192.168.242.1"))
	assert.Equal(err, nil, "Error getting routes")
	assert.True(routes[0].Src.Equal(net.ParseIP("192.168.242.6")), "IP not what was expected")

	n0.disconnect()
	checkLogs(hook.Entries, t)
}

func TestMultipleConnectWarn(t *testing.T) {
	n0r := createNetwork(0, true, t)
	defer removeNetwork(n0r.ID, t)

	hook := logtest.NewGlobal()
	n0 := newNetwork(&n0r)
	n0.connect()
	defer n0.disconnect()
	checkLogs(hook.Entries, t)

	n0.connect()
	// we should not get warnings anymore
	checkLogs(hook.Entries, t)
}

func TestIsConnected(t *testing.T) {
	assert := assert.New(t)

	n0r := createNetwork(0, true, t)
	defer removeNetwork(n0r.ID, t)

	n0 := newNetwork(&n0r)
	hook := logtest.NewGlobal()
	assert.False(n0.isConnected(), "Network should not be connected")
	checkLogs(hook.Entries, t)

	n0.connect()
	hook.Reset()
	assert.True(n0.isConnected(), "Network should be connected")
	checkLogs(hook.Entries, t)

	n0.disconnect()
	assert.False(n0.isConnected(), "Network should not be connected")
	checkLogs(hook.Entries, t)
}

func TestIsDrouterTrue(t *testing.T) {
	assert := assert.New(t)

	n0r := createNetwork(0, true, t)
	defer removeNetwork(n0r.ID, t)
	n0 := newNetwork(&n0r)

	assert.True(n0.isDRouter())
}

func TestIsDrouterFalse(t *testing.T) {
	assert := assert.New(t)

	n0r := createNetwork(0, false, t)
	defer removeNetwork(n0r.ID, t)
	n0 := newNetwork(&n0r)

	assert.False(n0.isDRouter())
}

func TestIsDrouterTransit(t *testing.T) {
	assert := assert.New(t)

	n0r := createNetwork(0, true, t)
	defer removeNetwork(n0r.ID, t)
	transitNetID = n0r.ID
	defer func() { transitNetID = "" }()
	n0 := newNetwork(&n0r)

	assert.True(n0.isDRouter())
}

func TestAdminDownNonAggressive(t *testing.T) {
	assert := assert.New(t)

	n0r := createNetwork(0, true, t)
	defer removeNetwork(n0r.ID, t)
	transitNetID = n0r.ID
	defer func() { transitNetID = "" }()
	n0 := newNetwork(&n0r)

	assert.False(n0.adminDown)
	err := n0.disconnectEvent()
	assert.Equal(err, nil, "Error with disconnectEvent")
	assert.False(n0.adminDown, "adminDown should be False")
}

func TestAdminDownAggressive(t *testing.T) {
	assert := assert.New(t)
	aggressive = true
	defer func() { aggressive = false }()

	n0r := createNetwork(0, true, t)
	defer removeNetwork(n0r.ID, t)
	transitNetID = n0r.ID
	defer func() { transitNetID = "" }()
	n0 := newNetwork(&n0r)

	assert.False(n0.adminDown)
	err := n0.disconnectEvent()
	assert.Equal(err, nil, "Error with disconnectEvent")
	assert.True(n0.adminDown, "adminDown should be True")

	err = n0.connectEvent()
	assert.Equal(err, nil, "Error with connectEvent")
	assert.False(n0.adminDown, "adminDown should be False")
}

func createMultiSubnetNetwork(n int, dr bool, t *testing.T) dockerTypes.NetworkResource {
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
				dockerNTypes.IPAMConfig{
					Subnet:  fmt.Sprintf("192.168.243.%v/29", n*8),
					Gateway: fmt.Sprintf("192.168.243.%v", n*8+1),
				},
			},
		},
	})
	require.Equal(t, err, nil, "Error creating network")
	nr, err := dc.NetworkInspect(bg, r.ID)
	require.Equal(t, err, nil, "Error inspecting network")
	return nr
}

// disabled becasue bridge driver doesn't support multiple subnets
func testMultiSubnetIPOffset(t *testing.T) {
	assert := assert.New(t)
	ipOffset = 2
	defer func() { ipOffset = 0 }()

	n0r := createMultiSubnetNetwork(0, true, t)
	defer removeNetwork(n0r.ID, t)

	hook := logtest.NewGlobal()
	n0 := newNetwork(&n0r)
	n0.connect()

	routes, err := netlink.RouteGet(net.ParseIP("192.168.242.1"))
	assert.Equal(err, nil, "Error getting routes")

	ip1 := false
	ip2 := false
	for _, r := range routes {
		if r.Src.Equal(net.ParseIP("192.168.242.2")) {
			ip1 = true
		}
		if r.Src.Equal(net.ParseIP("192.168.243.2")) {
			ip2 = true
		}
	}

	assert.True(ip1, "First ip not set")
	assert.True(ip2, "Second ip not set")

	n0.disconnect()
	checkLogs(hook.Entries, t)
}
