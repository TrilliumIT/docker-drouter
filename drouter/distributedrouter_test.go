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

func cleanup() {
	dockerNets, err := dc.NetworkList(bg, dockerTypes.NetworkListOptions{})
	if err != nil {
		return
	}
	for _, dn := range dockerNets {
		if strings.HasPrefix(dn.Name, "drntest_n") {
			for c := range dn.Containers {
				dc.NetworkDisconnect(bg, dn.ID, c, true)
			}
			dc.NetworkRemove(bg, dn.ID)
		}
	}
}

func TestMain(m *testing.M) {
	exitStatus := 1
	defer func() { os.Exit(exitStatus) }()
	var err error

	opts := &DistributedRouterOptions{
		InstanceName: DR_INST,
	}
	err = initVars(opts)
	if err != nil {
		return
	}
	dc = dockerClient
	bg = context.Background()

	cleanup()

	testNets = make([]*net.IPNet, 4)
	for i, n := range testNets {
		_, n, _ := net.ParseCIDR(fmt.Sprintf(NET_IPNET, i*8))
	}

	exitStatus = m.Run()

	cleanup()

	// rejoin bridge, required to upload coverage
	dc.NetworkConnect(bg, "bridge", selfContainerID, &dockerNTypes.EndpointSettings{})
}

func checkLogs(entries []*log.Entry, t *testing.T) {
	for _, e := range entries {
		assert.Equal(t, e.Level <= log.InfoLevel, true, "All logs should be <= Info")
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
	assert := assert.New(t)
	require := require.New(t)
	hook := logtest.NewGlobal()

	n0r := createNetwork(0, false, t)
	defer removeNetwork(n0r.ID, t)
	n1r := createNetwork(1, true, t)
	defer removeNetwork(n1r.ID, t)
	n2r := createNetwork(2, true, t)
	defer removeNetwork(n2r.ID, t)

	c0 := createContainer(n0r.Name, t)
	defer removeContainer(c0, t)
	c1 := createContainer(n1r.Name, t)
	defer removeContainer(c1, t)

	quit := make(chan struct{})
	ech := make(chan error)
	go func() {
		ech <- Run(opts, quit)
	}()

	startDelay := time.NewTimer(10 * time.Second)
	var err error
	select {
	case _ = <-startDelay.C:
		err = nil
	case err = <-ech:
	}

	require.Equal(err, nil, "Run() returned an error.")
	checkLogs(hook.Entries, t)

	for _, e := range hook.Entries {
		if e.Level >= log.InfoLevel {
			continue
		}

		if d, ok := e.Data["container"]; ok {
			assert.NotEqual(d["ID"], c0, "There should be no >= INFO logs for c0.")
		}
	}

	c1c, err := newContainerFromID(c1)
	assert.Equal(err, nil, "Failed to create c1.")

	if aggressive {
		assert.True(handleContainsRoute(c1c.handle, testNets[2], nil, t), "c1 should have a route to n2.")
	}

	c2 := createContainer(n2r.Name, t)
	defer removeContainer(c2, t)
	time.Sleep(5 * time.Second)

	checkLogs(hook.Entries, t)

	assert.False(handleContainsRoute(c1c.handle, testNets[0], nil, t), "c1 should not have a route to n0.")
	assert.True(handleContainsRoute(c1c.handle, testNets[2], nil, t), "c1 should have a route to n2.")

	c2c, err := newContainerFromID(c1)
	assert.Equal(err, nil, "Failed to create c2.")

	assert.False(handleContainsRoute(c2c.handle, testNets[0], nil, t), "c2 should not have a route to n0.")
	assert.True(handleContainsRoute(c2c.handle, testNets[1], nil, t), "c2 should have a route to n1.")

	n3r := createNetwork(3, true, t)
	defer removeNetwork(n3r.ID, t)

	//only sleep if aggressive to give drouter time to connect to those networks
	if aggressive {
		time.Sleep(10 * time.Second)
		assert.True(handleContainsRoute(c1c.handle, testNets[3], nil, t), "c1 should have a route to n3.")
		assert.True(handleContainsRoute(c2c.handle, testNets[3], nil, t), "c2 should have a route to n3.")
	} else {
		assert.False(handleContainsRoute(c1c.handle, testNets[3], nil, t), "c1 should not have a route to n3 yet.")
		assert.False(handleContainsRoute(c2c.handle, testNets[3], nil, t), "c2 should not have a route to n3 yet.")
	}

	close(quit)
}

func handleContainsRoute(h *netlink.Handle, to *net.IPNet, via *net.IP, t *testing.T) bool {
	require := require.New(t)

	routes, err := h.RouteList(nil, netlink.FAMILY_ALL)
	require.Equal(err, nil, "Failed to get routes from handle.")

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

/*
// startup & shutdown tests
n0 := non drouter network
c0 := container on n0
n1 := drouter network
c1 := container on n1
n2 := drouter network
c2 := container on n2
n3 := drouter network

start drouter

docker events expected:
	- self connect to n1
	- self connect to n2
	if aggresive:
		- self connect to n3

route events expected:
	- self direct to n1
	- self direct to n2
	if aggressive:
		- self direct to n3
	if containergateway:
		- gateway change on c1
		- gateway change on c2
	if !containergateway:
		- c1 to n2 via self-n1 - after direct to n1
		- c2 to n1 via self-n2 - after direct to n2
		if aggressive:
			- c1 to n3 via self-n1 - after direct to n3
			- c2 to n3 via self-n2 - after direct to n3

stop drouter

docker events expected:
	- self disconnect from n1
	- self disconnect from n2
	if aggressive:
		- self disconnect from n3

route events expected:
	// remember we don't see route loss on interface down
	if containergateway:
		- gateway change to orig on c1
		- gateway change to orig on c2
	if !containergateway:
		- lose c1 to n2 - before self disconnect on n2
		- lose c2 to n1 - before self disconnect on n1
		if aggressive:
			- lose c1 to n3 - before self disconnect on n3
			- lose c2 to n3 - before self disconnect on n3

// multi-connected tests


// multi-subnet on dockernet test

*/
