package drouter

import (
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

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
	DrInst = "dr_test"
)

func TestMain(m *testing.M) {
	fmt.Println("Begin TestMain().")
	exitStatus := 1
	defer func() { os.Exit(exitStatus) }()

	testNets = make([]*net.IPNet, 4)
	for i := range testNets {
		_, n, _ := net.ParseCIDR(fmt.Sprintf(NetIPNet, i*8))
		testNets[i] = n
	}

	exitStatus = m.Run()

	err := cleanup()
	if err != nil {
		panic(err)
	}

	// rejoin bridge, required to upload coverage
	err = dc.NetworkConnect(bg, "bridge", selfContainerID, &dockerNTypes.EndpointSettings{})
	if err != nil {
		panic(err)
	}
	fmt.Println("End TestMain().")
}

func defaultOpts() *DistributedRouterOptions {
	return &DistributedRouterOptions{
		IPOffset:         0,
		Aggressive:       true,
		HostShortcut:     false,
		ContainerGateway: false,
		HostGateway:      false,
		Masquerade:       false,
		P2PAddr:          "172.29.255.252/30",
		StaticRoutes:     make([]string, 0),
		TransitNet:       "",
		InstanceName:     DrInst,
	}
}

func TestDefault(t *testing.T) {
	require.NoError(t, cleanup(), "Failed to cleanup()")

	assert := assert.New(t)
	require := require.New(t)
	hook := logtest.NewGlobal()

	opts := defaultOpts()

	st := simulation{
		opts:    opts,
		assert:  assert,
		require: require,
		cb:      make(map[int]func()),
	}

	st.cb[assertInit] = func() {
		checkLogs(hook, assert)

		drn, ok := st.dr.getNetwork(st.n[0].ID)
		assert.True(ok, "should have learned n0 by now.")

		drn, ok = st.dr.getNetwork(st.n[2].ID)
		assert.True(ok, "should have learned n2 by now.")
		assert.True(drn.isConnected(), "drouter should be connected to n2.")
	}

	require.NoError(st.runV4(), "Scenario failed to run.")
}

func TestNoAggressive(t *testing.T) {
	require.NoError(t, cleanup(), "Failed to cleanup()")

	assert := assert.New(t)
	require := require.New(t)
	hook := logtest.NewGlobal()

	opts := defaultOpts()
	opts.Aggressive = false

	st := simulation{
		opts:    opts,
		assert:  assert,
		require: require,
		cb:      make(map[int]func()),
	}

	st.cb[assertInit] = func() {
		warns := 0
		for _, e := range hook.Entries {
			if e.Level == log.WarnLevel {
				warns++
			}
			assert.True(e.Level >= log.WarnLevel, "All logs should be >= Warn, but observed log: ", e)
		}
		hook.Reset()
		assert.Equal(warns, 1, "Should have recieved one warning message for running in Aggressive with no tranist net")

		if drn, ok := st.dr.getNetwork(st.n[2].ID); ok {
			assert.False(drn.isConnected(), "drouter should not be connected to n2.")
		}
	}

	require.NoError(st.runV4(), "Scenario failed to run.")
}

func cleanup() error {
	resetGlobals()

	dockerContainers, _ := dc.ContainerList(bg, dockerTypes.ContainerListOptions{All: true})
	for _, c := range dockerContainers {
		for _, name := range c.Names {
			if strings.HasPrefix(name, "/drntest_c") {
				err := dc.ContainerKill(bg, c.ID, "")
				if err != nil && !strings.Contains(err.Error(), "is not running") {
					return err
				}
				err = dc.ContainerRemove(bg, c.ID, dockerTypes.ContainerRemoveOptions{Force: true})
				if err != nil {
					return err
				}
			}
		}
	}
	dockerNets, _ := dc.NetworkList(bg, dockerTypes.NetworkListOptions{})
	for _, dn := range dockerNets {
		if strings.HasPrefix(dn.Name, "drntest_n") {
			for c := range dn.Containers {
				err := dc.NetworkDisconnect(bg, dn.ID, c, true)
				if err != nil {
					return err
				}
			}
			err := dc.NetworkRemove(bg, dn.ID)
			if err != nil {
				return err
			}
		}
	}
	return nil
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
	err := initVars(defaultOpts())
	if err != nil {
		panic(err)
	}

	dc = dockerClient
	bg = context.Background()
}

func checkLogs(hook *logtest.Hook, assert *assert.Assertions) {
	for _, e := range hook.Entries {
		assert.True(e.Level >= log.InfoLevel, "All logs should be >= Info, but observed log: ", e)
	}

	hook.Reset()
}

func handleContainsRoute(h *netlink.Handle, to *net.IPNet, via *net.IP, assert *assert.Assertions) bool {
	routes, err := h.RouteList(nil, netlink.FAMILY_ALL)
	assert.NoError(err, "Failed to get routes from handle.")

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
