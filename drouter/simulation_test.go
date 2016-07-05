package drouter

import (
	"fmt"
	"time"

	logtest "github.com/Sirupsen/logrus/hooks/test"
	dockerTypes "github.com/docker/engine-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

const (
	assertInit = iota

//	assertC2Start
//	assertN3Add
)

type simulation struct {
	dr       *distributedRouter
	opts     *DistributedRouterOptions
	c        []*container
	n        []*dockerTypes.NetworkResource
	cb       map[int]func()
	assert   *assert.Assertions
	require  *require.Assertions
	c0routes []netlink.Route
}

func (st *simulation) runV4() error {
	st.c = make([]*container, 4)
	st.n = make([]*dockerTypes.NetworkResource, 4)
	hook := logtest.NewGlobal()
	var err error

	//create first 3 networks
	fmt.Println("Creating networks 0, 1, and 2.")

	for i := 0; i < 3; i++ {
		st.n[i], err = createNetwork(i, i != 0)
		st.require.NoError(err, "Failed to create n%v.", i)
		//defer func() { st.require.NoError(dc.NetworkRemove(bg, st.n[i].ID), "Failed to remove n%v.", i) }()
	}

	//create first 2 containers
	fmt.Println("Creating containers 0-1.")
	for i := 0; i < 2; i++ {
		st.c[i], err = createContainer(i, st.n[i].ID)
		//defer func(c *container) { st.assert.NoError(c.remove(), "Failed to remove c%v.", i) }(c[i], i)
		st.require.NoError(err, "Failed to get container object for c%v.", i)
	}

	st.c0routes, err = st.c[0].handle.RouteList(nil, netlink.FAMILY_V4)
	st.require.NoError(err, "Failed to get c0 initial routes.")

	//Get DRouter going
	quit := make(chan struct{})
	stopChan = quit

	st.dr, err = newDistributedRouter(st.opts)
	st.require.NoError(err, "Failed to create dr object.")

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

	//assertInit callback
	st.cb[assertInit]()

	st.checkC0Routes()

	if drn, ok := st.dr.getNetwork(st.n[0].ID); ok {
		st.assert.False(drn.isConnected(), "drouter should not be connected to n0.")
	}

	drn, ok := st.dr.getNetwork(st.n[1].ID)
	st.assert.True(ok, "should have learned n1 by now.")
	st.assert.True(drn.isConnected(), "drouter should be connected to n1.")

	st.assert.Equal(aggressive, handleContainsRoute(st.c[1].handle, testNets[2], nil, st.assert), "c1 should have a route to n2 if in aggressive mode.")

	st.c[2], err = createContainer(2, st.n[2].ID)
	st.require.NoError(err, "Failed to create c2.")
	time.Sleep(5 * time.Second)

	checkLogs(hook, st.assert)

	st.assert.False(handleContainsRoute(st.c[1].handle, testNets[0], nil, st.assert), "c1 should not have a route to n0.")
	st.assert.True(handleContainsRoute(st.c[1].handle, testNets[2], nil, st.assert), "c1 should have a route to n2.")

	st.assert.False(handleContainsRoute(st.c[2].handle, testNets[0], nil, st.assert), "c2 should not have a route to n0.")
	st.assert.True(handleContainsRoute(st.c[2].handle, testNets[1], nil, st.assert), "c2 should have a route to n1.")

	st.n[3], err = createNetwork(3, true)
	st.assert.NoError(err, "Failed to create n3.")
	//defer func() { st.assert.NoError(dc.NetworkRemove(bg, st.n[3].ID), "Failed to remove n3.") }()

	//sleep to give aggressive time to connect to n3
	time.Sleep(10 * time.Second)

	st.assert.Equal(aggressive, handleContainsRoute(st.c[1].handle, testNets[3], nil, st.assert), "c1 should have a route to n3 if in aggressive.")
	st.assert.Equal(aggressive, handleContainsRoute(st.c[2].handle, testNets[3], nil, st.assert), "c2 should have a route to n3 if in aggressive.")

	// purposefully remove c2 and make sure c1 looses the route in non-aggressive
	st.assert.NoError(st.c[2].remove(), "Failed to remove c2.")
	time.Sleep(5 * time.Second)
	checkLogs(hook, st.assert)

	st.assert.Equal(aggressive, handleContainsRoute(st.c[1].handle, testNets[2], nil, st.assert), "c1 should have a route to n2 in aggressive mode.")

	close(quit)

	st.assert.NoError(<-ech, "Error during drouter shutdown.")
	checkLogs(hook, st.assert)

	return nil
}

func (st *simulation) checkC0Routes() {
	c0newRoutes, err := st.c[0].handle.RouteList(nil, netlink.FAMILY_V4)
	st.require.NoError(err, "Failed to get c0 routes after dr start.")
	st.assert.EqualValues(st.c0routes, c0newRoutes, "Should not modify c0 routes.")
}
