package drouter

import (
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TrilliumIT/iputil"
	dockerclient "github.com/docker/engine-api/client"
	dockerTypes "github.com/docker/engine-api/types"
	dockerNTypes "github.com/docker/engine-api/types/network"
	"golang.org/x/net/context"

	log "github.com/Sirupsen/logrus"
	logtest "github.com/Sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

var (
	dc       *dockerclient.Client
	bg       context.Context
	testNets []*net.IPNet
)

const (
	DR_INST = "dr_test"
)

func TestMain(m *testing.M) {
	fmt.Println("Begin TestMain().")
	exitStatus := 1
	defer func() { os.Exit(exitStatus) }()

	resetGlobals()
	cleanup()

	testNets = make([]*net.IPNet, 4)
	for i, _ := range testNets {
		_, n, _ := net.ParseCIDR(fmt.Sprintf(NET_IPNET, i*8))
		testNets[i] = n
	}

	exitStatus = m.Run()

	cleanup()

	// rejoin bridge, required to upload coverage
	dc.NetworkConnect(bg, "bridge", selfContainerID, &dockerNTypes.EndpointSettings{})
	fmt.Println("End TestMain().")
}

func TestBasicAggressive(t *testing.T) {
	opts := &DistributedRouterOptions{
		IPOffset:         0,
		Aggressive:       true,
		HostShortcut:     false,
		ContainerGateway: false,
		HostGateway:      false,
		Masquerade:       false,
		P2PAddr:          "172.29.255.252/30",
		StaticRoutes:     make([]string, 0),
		TransitNet:       "",
		InstanceName:     DR_INST,
	}
	cleanup()
	runScenarioV4(opts, t)
	cleanup()
}

func TestBasicNonAggressive(t *testing.T) {
	opts := &DistributedRouterOptions{
		IPOffset:         0,
		Aggressive:       false,
		HostShortcut:     false,
		ContainerGateway: false,
		HostGateway:      false,
		Masquerade:       false,
		P2PAddr:          "172.29.255.252/30",
		StaticRoutes:     make([]string, 0),
		TransitNet:       "",
		InstanceName:     DR_INST,
	}
	cleanup()
	runScenarioV4(opts, t)
	cleanup()
}

func cleanup() {
	dockerContainers, _ := dc.ContainerList(bg, dockerTypes.ContainerListOptions{All: true})
	for _, c := range dockerContainers {
		for _, name := range c.Names {
			if strings.HasPrefix(name, "/drntest_c") {
				dc.ContainerKill(bg, c.ID, "")
				dc.ContainerRemove(bg, c.ID, dockerTypes.ContainerRemoveOptions{Force: true})
			}
		}
	}
	dockerNets, _ := dc.NetworkList(bg, dockerTypes.NetworkListOptions{})
	for _, dn := range dockerNets {
		if strings.HasPrefix(dn.Name, "drntest_n") {
			for c := range dn.Containers {
				dc.NetworkDisconnect(bg, dn.ID, c, true)
			}
			dc.NetworkRemove(bg, dn.ID)
		}
	}
}

func resetGlobals() {
	//cli options
	ipOffset = 0
	aggressive = false
	hostShortcut = false
	containerGateway = false
	hostGateway = false
	masquerade = false
	p2p = nil
	staticRoutes = nil
	transitNetName = ""
	selfContainerID = ""
	instanceName = ""

	//other globals
	dockerClient = nil
	transitNetID = ""
	stopChan = nil

	log.SetLevel(log.InfoLevel)

	//re-init
	opts := &DistributedRouterOptions{
		Aggressive:   true,
		InstanceName: DR_INST,
	}
	initVars(opts)

	dc = dockerClient
	bg = context.Background()
}

func checkLogs(entries []*log.Entry, t *testing.T) {
	for _, e := range entries {
		assert.True(t, e.Level >= log.InfoLevel, "All logs should be >= Info, but observed log: ", e)
	}
}

// A most basic test to make sure it doesn't die on start
func testRunClose(t *testing.T) {
	opts := &DistributedRouterOptions{
		Aggressive: true,
	}

	quit := make(chan struct{})
	ech := make(chan error)
	go func() {
		ech <- Run(opts, quit)
	}()

	timeout := time.NewTimer(10 * time.Second)
	var err error
	select {
	case _ = <-timeout.C:
		close(quit)
		err = <-ech
	case err = <-ech:
	}

	if err != nil {
		t.Errorf("Error on Run Return: %v", err)
	}
}

func runScenarioV4(opts *DistributedRouterOptions, t *testing.T) {
	defer resetGlobals()
	var err error

	//Temporary debug level to write this function
	log.SetLevel(log.DebugLevel)

	assert := assert.New(t)
	require := require.New(t)
	hook := logtest.NewGlobal()

	fmt.Println("Creating networks 0-2.")
	n0r := createNetwork(0, false, t)
	defer removeNetwork(n0r.ID, t)
	n1r := createNetwork(1, true, t)
	defer removeNetwork(n1r.ID, t)
	n2r := createNetwork(2, true, t)
	defer removeNetwork(n2r.ID, t)

	fmt.Println("Creating containers 0-1.")
	c0 := createContainer("0", n0r.Name, t)
	defer removeContainer(c0, t)

	c0c, err := newContainerFromID(c0)
	assert.NoError(err, "Failed to get container object for c0.")
	c0routes, err := c0c.handle.RouteList(nil, netlink.FAMILY_ALL)
	assert.NoError(err, "Failed to get c0 route list.")

	c1 := createContainer("1", n1r.Name, t)
	defer removeContainer(c1, t)

	quit := make(chan struct{})
	stopChan = quit

	dr, err := newDistributedRouter(opts)
	require.NoError(err, "Failed to create dr object.")

	ech := make(chan error)
	go func() {
		fmt.Println("Starting DRouter.")
		ech <- dr.start()
	}()

	startDelay := time.NewTimer(20 * time.Second)
	select {
	case _ = <-startDelay.C:
		err = nil
	case err = <-ech:
	}
	fmt.Println("DRouter started.")

	require.NoError(err, "Run() returned an error.")
	if !aggressive && transitNetName == "" {
		warns := 0
		for _, e := range hook.Entries {
			if e.Level == log.WarnLevel {
				warns += 1
			}
			assert.True(e.Level >= log.WarnLevel, "All logs should be >= Warn, but observed log: ", e)
		}
		hook.Reset()
		assert.Equal(warns, 1, "Should have recieved one warning message for running in Aggressive with no tranist net")
	} else {
		checkLogs(hook.Entries, t)
	}

	drn, ok := dr.getNetwork(n1r.ID)
	assert.True(ok, "should have learned n1 by now.")
	assert.True(drn.isConnected(), "drouter should be connected to n1.")

	drn, ok = dr.getNetwork(n0r.ID)
	if aggressive {
		assert.True(ok, "should have learned n0 by now.")
	}
	// it's okay if we learned it in non-aggressive mode, but we should only be connected if we're in aggressive mode
	if ok {
		assert.False(drn.isConnected(), "drouter should not be connected to n0.")
	}

	drn, ok = dr.getNetwork(n2r.ID)
	if aggressive {
		assert.True(ok, "should have learned n2 by now.")
	}
	if ok {
		assert.Equal(aggressive, drn.isConnected(), "drouter should be connected to n2 if in aggressive mode.")
	}

	c0newRoutes, err := c0c.handle.RouteList(nil, netlink.FAMILY_ALL)
	assert.NoError(err, "Failed to get new c0 route list.")
	assert.EqualValues(c0routes, c0newRoutes, "c0 routes should not be modified.")

	c1c, err := newContainerFromID(c1)
	assert.NoError(err, "Failed to get container object for c1.")

	assert.Equal(aggressive, handleContainsRoute(c1c.handle, testNets[2], nil, t), "c1 should have a route to n2 if in aggressive mode.")

	c2 := createContainer("2", n2r.Name, t)
	time.Sleep(5 * time.Second)

	checkLogs(hook.Entries, t)

	assert.False(handleContainsRoute(c1c.handle, testNets[0], nil, t), "c1 should not have a route to n0.")
	assert.True(handleContainsRoute(c1c.handle, testNets[2], nil, t), "c1 should have a route to n2.")

	c2c, err := newContainerFromID(c2)
	assert.NoError(err, "Failed to create c2.")

	assert.False(handleContainsRoute(c2c.handle, testNets[0], nil, t), "c2 should not have a route to n0.")
	assert.True(handleContainsRoute(c2c.handle, testNets[1], nil, t), "c2 should have a route to n1.")

	n3r := createNetwork(3, true, t)
	defer removeNetwork(n3r.ID, t)

	//sleep to give aggressive time to connect to those networks
	time.Sleep(10 * time.Second)
	assert.Equal(aggressive, handleContainsRoute(c1c.handle, testNets[3], nil, t), "c1 should have a route to n3 if in aggressive.")
	assert.Equal(aggressive, handleContainsRoute(c2c.handle, testNets[3], nil, t), "c2 should have a route to n3 if in aggressive.")

	// purposefully remove c2 and make sure c1 looses the route in non-aggressive
	removeContainer(c2, t)
	time.Sleep(5 * time.Second)
	checkLogs(hook.Entries, t)

	assert.Equal(aggressive, handleContainsRoute(c1c.handle, testNets[2], nil, t), "c1 should have a route to n2 in aggressive mode.")

	close(quit)

	assert.NoError(<-ech, "Error during drouter shutdown.")
	checkLogs(hook.Entries, t)
}

func handleContainsRoute(h *netlink.Handle, to *net.IPNet, via *net.IP, t *testing.T) bool {
	require := require.New(t)

	routes, err := h.RouteList(nil, netlink.FAMILY_ALL)
	require.NoError(err, "Failed to get routes from handle.")

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
