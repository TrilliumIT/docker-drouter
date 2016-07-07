package drouter

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/TrilliumIT/iputil"
	dockerTypes "github.com/docker/engine-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

type simulation struct {
	dr         *distributedRouter
	c          []*container
	n          []*dockerTypes.NetworkResource
	hns        *netlink.Handle
	cb         *simCallbacks
	assert     *assert.Assertions
	require    *require.Assertions
	c0routes   []netlink.Route
	c1routes   []netlink.Route
	hostRoutes []netlink.Route
}

type simCallbacks struct {
	assertInit     func()
	assertC2Start  func()
	assertN3Add    func()
	assertC2Stop   func()
	assertN3Remove func()
	assertDeinit   func()
}

func newSimCallbacks() *simCallbacks {
	return &simCallbacks{
		assertInit:     func() {},
		assertC2Start:  func() {},
		assertN3Add:    func() {},
		assertC2Stop:   func() {},
		assertN3Remove: func() {},
		assertDeinit:   func() {},
	}
}

func newSimulation(opts *DistributedRouterOptions, t *testing.T) (*simulation, error) {
	assert := assert.New(t)
	require := require.New(t)

	hns, err := netlinkHandleFromPid(1)
	if err != nil {
		return nil, err
	}

	dr, err := newDistributedRouter(opts)
	if err != nil {
		return nil, err
	}

	return &simulation{
		dr:      dr,
		c:       make([]*container, 4),
		n:       make([]*dockerTypes.NetworkResource, 4),
		assert:  assert,
		require: require,
		hns:     hns,
		cb:      newSimCallbacks(),
	}, nil
}

func (st *simulation) runV4() error {
	var err error

	//capture host routes before anything
	st.hostRoutes, err = st.hns.RouteList(nil, netlink.FAMILY_V4)
	st.require.NoError(err, "Failed to get host initial routes.")

	//create first 3 networks
	fmt.Println("Creating networks 0, 1, and 2.")

	for i := 0; i < 3; i++ {
		st.n[i], err = createNetwork(i, i != 0)
		st.require.NoError(err, "Failed to create n%v.", i)
		defer func(n *dockerTypes.NetworkResource) {
			st.require.NoError(dc.NetworkRemove(bg, n.ID), "Failed to remove %v.", n.Name)
		}(st.n[i])
	}

	//create first 2 containers
	fmt.Println("Creating containers 0-1.")
	for i := 0; i < 2; i++ {
		st.c[i], err = createContainer(i, st.n[i].Name)
		st.require.NoError(err, "Failed to get container object for c%v.", i)
		defer func(c *container) { st.assert.NoError(c.remove(), "Failed to remove %v.", c.id) }(st.c[i])
	}

	st.c0routes, err = st.c[0].handle.RouteList(nil, netlink.FAMILY_V4)
	st.require.NoError(err, "Failed to get c0 initial routes.")

	st.c1routes, err = st.c[1].handle.RouteList(nil, netlink.FAMILY_V4)
	st.require.NoError(err, "Failed to get c1 initial routes.")

	//Get DRouter going
	quit := make(chan struct{})
	stopChan = quit

	ech := make(chan error)
	go func() {
		fmt.Println("Starting DRouter.")
		ech <- st.dr.start()
	}()

	startDelay := time.NewTimer(10 * time.Second)
	select {
	case <-startDelay.C:
		err = nil
	case err = <-ech:
	}
	fmt.Println("DRouter started.")
	st.require.NoError(err, "Run() returned an error.")

	//check c0 routes
	st.checkC0Routes()

	//global initial assertions
	if drn, ok := st.dr.getNetwork(st.n[0].ID); ok {
		st.assert.False(drn.isConnected(), "drouter should not be connected to n0.")
	}

	drn, ok := st.dr.getNetwork(st.n[1].ID)
	st.assert.True(ok, "should have learned n1 by now.")
	st.assert.True(drn.isConnected(), "drouter should be connected to n1.")

	if drn, ok = st.dr.getNetwork(st.n[2].ID); ok {
		st.assert.Equal(aggressive, drn.isConnected(), "drouter should be connected to n2 in aggressive mode.")
	}

	//DRouter init callback assertions
	st.cb.assertInit()
	checkLogs(st.assert)

	//EVENT: create c2
	st.c[2], err = createContainer(2, st.n[2].Name)
	st.require.NoError(err, "Failed to create c2.")
	time.Sleep(5 * time.Second)

	//check c0 routes
	st.checkC0Routes()

	//global c2start assertions
	drn, ok = st.dr.getNetwork(st.n[1].ID)
	st.assert.True(ok, "should have learned n1 by now.")
	st.assert.True(drn.isConnected(), "drouter should be connected to n1.")

	drn, ok = st.dr.getNetwork(st.n[2].ID)
	st.assert.True(ok, "should have learned n2 by now.")
	st.assert.True(drn.isConnected(), "drouter should be connected to n2.")

	//c2 start callback assertions
	st.cb.assertC2Start()
	checkLogs(st.assert)

	//EVENT: create n3
	st.n[3], err = createNetwork(3, true)
	st.assert.NoError(err, "Failed to create n3.")
	//sleep to give aggressive time to connect to n3
	time.Sleep(10 * time.Second)

	//check c0 routes
	st.checkC0Routes()

	//global n3add assertions
	if drn, ok = st.dr.getNetwork(st.n[3].ID); ok {
		st.assert.Equal(aggressive, drn.isConnected(), "drouter should be connected to n3 in aggressive mode.")
	}

	//n3add callback assertions
	st.cb.assertN3Add()
	checkLogs(st.assert)

	//EVENT: disconnect from n3, then remove it
	//we have to disconnect first, because so would an admin
	if drn, ok = st.dr.getNetwork(st.n[3].ID); ok && drn.isConnected() {
		st.require.NoError(dc.NetworkDisconnect(bg, st.n[3].ID, selfContainerID, false), "Failed to disconnect drouter from n3.")
		time.Sleep(5 * time.Second)
		st.assert.True(drn.adminDown, "The adminDown flag should be true after a manual disconnect.")
	}

	//admin now deletes the network
	st.assert.NoError(dc.NetworkRemove(bg, st.n[3].ID))
	time.Sleep(5 * time.Second)

	//check c0 routes
	st.checkC0Routes()

	//global n3remove assertions
	drn, ok = st.dr.getNetwork(st.n[1].ID)
	st.assert.True(ok, "should have learned n1 by now.")
	st.assert.True(drn.isConnected(), "drouter should still be connected to n1.")

	drn, ok = st.dr.getNetwork(st.n[2].ID)
	st.assert.True(ok, "should have learned n2 by now.")
	st.assert.True(drn.isConnected(), "drouter should still be connected to n2.")

	drn, ok = st.dr.getNetwork(st.n[3].ID)
	if ok {
		st.assert.False(drn.isConnected(), "drouter should not be connected to n3.")
	}

	//n3remove callbacks
	st.cb.assertN3Remove()
	checkLogs(st.assert)

	//EVENT: stop c2
	st.assert.NoError(st.c[2].remove(), "Failed to remove c2.")
	time.Sleep(5 * time.Second)

	//check c0 routes
	st.checkC0Routes()

	if drn, ok = st.dr.getNetwork(st.n[2].ID); ok {
		st.assert.Equal(aggressive, drn.isConnected(), "drouter should still be connected to n2 in aggressive mode.")
	}

	//c2 stop callback
	st.cb.assertC2Stop()
	checkLogs(st.assert)

	st.assert.Equal(aggressive, st.handleContainsRoute(st.c[1].handle, testNets[2], nil), "c1 should have a route to n2 in aggressive mode.")

	//EVENT: Now test quitting
	fmt.Println("Stopping DRouter.")
	close(quit)

	st.assert.NoError(<-ech, "Error during drouter shutdown.")
	fmt.Println("DRouter stopped.")
	time.Sleep(5 * time.Second)

	//check host routes
	hostNewRoutes, err := st.hns.RouteList(nil, netlink.FAMILY_V4)
	st.require.NoError(err, "Failed to get host routes after dr stop.")
	st.assert.EqualValues(st.hostRoutes, hostNewRoutes, "Host routes should be returned to original state.")

	//check c0 routes
	st.checkC0Routes()

	//check c1 routes
	c1newRoutes, err := st.c[1].handle.RouteList(nil, netlink.FAMILY_V4)
	st.require.NoError(err, "Failed to get c1 routes after dr start.")
	st.assert.EqualValues(st.c1routes, c1newRoutes, "c1 routes should be returned to original state.")

	//TODO: more global quit assertions here

	//Deinit callback
	st.cb.assertDeinit()
	checkLogs(st.assert)

	return nil
}

func (st *simulation) checkC0Routes() {
	c0newRoutes, err := st.c[0].handle.RouteList(nil, netlink.FAMILY_V4)
	st.require.NoError(err, "Failed to get c0 routes after dr start.")
	st.assert.EqualValues(st.c0routes, c0newRoutes, "Should not modify c0 routes.")
}

func (st *simulation) handleContainsRoute(h *netlink.Handle, to *net.IPNet, via *net.IP) bool {
	routes, err := h.RouteList(nil, netlink.FAMILY_ALL)
	st.assert.NoError(err, "Failed to get routes from handle.")

	for _, r := range routes {
		if r.Dst == nil || r.Gw == nil {
			continue
		}

		if iputil.SubnetEqualSubnet(r.Dst, to) && (via == nil || r.Gw.Equal(*via)) {
			return true
		}
	}
	return false
}
