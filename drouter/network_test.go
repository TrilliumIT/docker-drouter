package drouter

import (
	"fmt"
	"net"
	"testing"

	log "github.com/Sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dockerTypes "github.com/docker/engine-api/types"
	dockerNTypes "github.com/docker/engine-api/types/network"
	"github.com/vishvananda/netlink"
)

const (
	NetName  = "drntest_n%v"
	NetGw    = "192.168.242.%v"
	NetIPNet = "192.168.242.%v/29"
)

func createNetwork(n int, dr bool) (*dockerTypes.NetworkResource, error) {
	name := fmt.Sprintf(NetName, n)
	opts := make(map[string]string)

	if dr {
		opts["drouter"] = DrInst
	}
	opts["com.docker.network.bridge.name"] = fmt.Sprintf("%v_%v", DrInst, n)

	r, err := dc.NetworkCreate(bg, name, dockerTypes.NetworkCreate{
		Options: opts,
		IPAM: &dockerNTypes.IPAM{
			Config: []dockerNTypes.IPAMConfig{
				{
					Subnet:  fmt.Sprintf(NetIPNet, n*8),
					Gateway: fmt.Sprintf(NetGw, n*8+1),
				},
			},
		},
	})
	if err != nil {
		return nil, err
	}

	nr, err := dc.NetworkInspect(bg, r.ID)
	if err != nil {
		return nil, err
	}

	err = removeAllAddrs(nr.Options["com.docker.network.bridge.name"])
	if err != nil {
		return nil, err
	}

	return &nr, nil
}

func removeAllAddrs(intName string) error {
	log.WithField("intName", intName).Debug("Removing all addresses")
	hns, _, err := netlinkHandleFromPid(1)
	if err != nil {
		return err
	}

	br, err := hns.LinkByName(intName)
	if err != nil {
		return err
	}

	addrs, err := hns.AddrList(br, netlink.FAMILY_ALL)
	if err != nil {
		return err
	}

	for _, addr := range addrs {
		err := hns.AddrDel(br, &addr)
		if err != nil {
			return err
		}
	}
	log.Debugf("%v addresses removed", len(addrs))

	return nil
}

func TestNetworkConnect(t *testing.T) {
	require.NoError(t, cleanup(), "Failed to cleanup()")

	assert := assert.New(t)
	require := require.New(t)

	n0r, err := createNetwork(0, true)
	require.NoError(err, "Failed to create n0.")
	defer func() { require.NoError(dc.NetworkRemove(bg, n0r.ID), "Failed to remove n0.") }()

	n0 := newNetwork(n0r)

	n0.connect()
	checkLogs(assert)

	n0.disconnect()
	checkLogs(assert)
}

func TestPositiveIPOffset(t *testing.T) {
	require.NoError(t, cleanup(), "Failed to cleanup()")

	testIPOffset(2, "192.168.242.2", t)
}

func TestNegativeIPOffset(t *testing.T) {
	require.NoError(t, cleanup(), "Failed to cleanup()")

	testIPOffset(-1, "192.168.242.6", t)
}

func testIPOffset(ipo int, exp string, t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ipOffset = ipo

	n0r, err := createNetwork(0, true)
	require.NoError(err, "Failed to create n0.")
	defer func() { require.NoError(dc.NetworkRemove(bg, n0r.ID), "Failed to remove n0.") }()

	n0 := newNetwork(n0r)
	n0.connect()
	defer n0.disconnect()

	routes, err := netlink.RouteGet(net.ParseIP("192.168.242.1"))
	require.NoError(err, "Error getting routes")
	assert.True(routes[0].Src.Equal(net.ParseIP(exp)), "IP not what was expected")

	checkLogs(assert)
}

func TestMultipleConnect(t *testing.T) {
	require.NoError(t, cleanup(), "Failed to cleanup()")

	assert := assert.New(t)
	require := require.New(t)

	n0r, err := createNetwork(0, true)
	require.NoError(err, "Failed to create n0.")
	defer func() { require.NoError(dc.NetworkRemove(bg, n0r.ID), "Failed to remove n0.") }()

	n0 := newNetwork(n0r)
	n0.connect()
	defer n0.disconnect()
	checkLogs(assert)

	n0.connect()

	checkLogs(assert)
}

func TestIsConnected(t *testing.T) {
	require.NoError(t, cleanup(), "Failed to cleanup()")

	assert := assert.New(t)
	require := require.New(t)

	n0r, err := createNetwork(0, true)
	require.NoError(err, "Failed to create n0.")
	defer func() { require.NoError(dc.NetworkRemove(bg, n0r.ID), "Failed to remove n0.") }()

	n0 := newNetwork(n0r)
	assert.False(n0.isConnected(), "Network should not be connected")
	checkLogs(assert)

	n0.connect()
	assert.True(n0.isConnected(), "Network should be connected")
	checkLogs(assert)

	n0.disconnect()
	assert.False(n0.isConnected(), "Network should not be connected")
	checkLogs(assert)
}

func TestIsDRouterTrue(t *testing.T) {
	require.NoError(t, cleanup(), "Failed to cleanup()")

	testIsDRouter(true, t)
}

func TestIsDRouterFalse(t *testing.T) {
	require.NoError(t, cleanup(), "Failed to cleanup()")

	testIsDRouter(false, t)
}

func testIsDRouter(flag bool, t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	n0r, err := createNetwork(0, flag)
	require.NoError(err, "Failed to create n0.")
	defer func() { require.NoError(dc.NetworkRemove(bg, n0r.ID), "Failed to remove n0.") }()

	n0 := newNetwork(n0r)

	assert.Equal(flag, n0.isDRouter(), "IsDRouter returns the wrong thing.")
}

func TestIsDRouterTransit(t *testing.T) {
	require.NoError(t, cleanup(), "Failed to cleanup()")

	assert := assert.New(t)
	require := require.New(t)

	n0r, err := createNetwork(0, false)
	require.NoError(err, "Failed to create n0.")
	defer func() { require.NoError(dc.NetworkRemove(bg, n0r.ID), "Failed to remove n0.") }()

	transitNetID = n0r.ID
	n0 := newNetwork(n0r)

	assert.True(n0.isDRouter())
}

func TestAdminDownNonAggressive(t *testing.T) {
	require.NoError(t, cleanup(), "Failed to cleanup()")

	assert := assert.New(t)
	require := require.New(t)

	n0r, err := createNetwork(0, true)
	require.NoError(err, "Failed to create n0.")
	defer func() { require.NoError(dc.NetworkRemove(bg, n0r.ID), "Failed to remove n0.") }()

	transitNetID = n0r.ID
	aggressive = false
	n0 := newNetwork(n0r)

	assert.False(n0.adminDown)
	assert.NoError(n0.disconnectEvent(), "Error with disconnectEvent")
	assert.False(n0.adminDown, "adminDown should be False")
}

func TestAdminDownAggressive(t *testing.T) {
	require.NoError(t, cleanup(), "Failed to cleanup()")

	assert := assert.New(t)
	require := require.New(t)

	n0r, err := createNetwork(0, true)
	require.NoError(err, "Failed to create n0.")
	defer func() { require.NoError(dc.NetworkRemove(bg, n0r.ID), "Failed to remove n0.") }()

	transitNetID = n0r.ID
	aggressive = true
	n0 := newNetwork(n0r)

	assert.False(n0.adminDown)
	assert.NoError(n0.disconnectEvent(), "Error with disconnectEvent")
	assert.True(n0.adminDown, "adminDown should be True")

	assert.NoError(n0.connectEvent(), "Error with connectEvent")
	assert.False(n0.adminDown, "adminDown should be False")
}
