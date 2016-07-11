package drouter

import (
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	dockerclient "github.com/docker/engine-api/client"
	dockerTypes "github.com/docker/engine-api/types"
	"golang.org/x/net/context"

	log "github.com/Sirupsen/logrus"
	logtest "github.com/Sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
)

var (
	dc       *dockerclient.Client
	bg       context.Context
	testNets []*net.IPNet
	hook     *logtest.Hook
)

const (
	DrInst = "dr_test"
)

func TestMain(m *testing.M) {
	exitStatus := 1
	defer func() { os.Exit(exitStatus) }()
	fmt.Println("Begin TestMain().")

	hook = logtest.NewGlobal()

	if os.Getenv("NO_TEST_SETUP") == "" {
		err := cleanup()
		if err != nil {
			return
		}
		err = disconnectDRFromEverything()
		if err != nil {
			return
		}
		time.Sleep(5 * time.Second)

		testNets = make([]*net.IPNet, 4)
		for i := range testNets {
			_, n, _ := net.ParseCIDR(fmt.Sprintf(NetIPNet, i*8))
			testNets[i] = n
		}
	}

	exitStatus = m.Run()

	if os.Getenv("NO_TEST_SETUP") == "" {
		err := cleanup()
		if err != nil {
			exitStatus = 1
			return
		}
	}
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
	hook.Reset()
}

func checkLogs(assert *assert.Assertions) {
	for _, e := range hook.Entries {
		assert.True(e.Level >= log.InfoLevel, "All logs should be >= Info, but observed log: ", e)
	}

	hook.Reset()
}
