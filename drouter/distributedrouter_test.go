package drouter

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	dockerclient "github.com/docker/engine-api/client"
	dockerTypes "github.com/docker/engine-api/types"
	dockerCTypes "github.com/docker/engine-api/types/container"
	dockerNTypes "github.com/docker/engine-api/types/network"
	"golang.org/x/net/context"

	log "github.com/Sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

var (
	dc *dockerclient.Client
	bg context.Context
)

func cleanup() {
	dockerNets, err := dc.NetworkList(bg, dockerTypes.NetworkListOptions{})
	if err != nil {
		return
	}
	for _, dn := range dockerNets {
		if strings.HasPrefix(dn.Name, "drntest_n") {
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

	timeoutCh := make(chan struct{})
	go func() {
		time.Sleep(10 * time.Second)
		close(timeoutCh)
	}()

	var err error
	select {
	case _ = <-timeoutCh:
		close(quit)
		err = <-ech
	case err = <-ech:
	}

	if err != nil {
		t.Errorf("Error on Run Return: %v", err)
	}
}

const (
	CONT_NAME  = "drntest_c%v"
	CONT_IMAGE = "alpine"
)

func createContainer(n int, t *testing.T) string {
	r, err := dc.ContainerCreate(bg,
		&dockerCTypes.Config{},
		&dockerCTypes.HostConfig{},
		&dockerNTypes.NetworkingConfig{
			EndpointsConfig: map[string]*dockerNTypes.EndpointSettings{
				fmt.Sprintf(NET_NAME, n): &dockerNTypes.EndpointSettings{},
			},
		}, "")
	if err != nil {
		t.Fatalf("Error creating container: %v", err)
	}

	err = dc.ContainerStart(bg, r.ID, dockerTypes.ContainerStartOptions{})

	if err != nil {
		containerRemove(r.ID, t)
		t.Fatalf("Error starting container: %v", err)
	}

	return r.ID
}

func containerRemove(id string, t *testing.T) {
	err := dc.ContainerRemove(bg, id, dockerTypes.ContainerRemoveOptions{})
	if err != nil {
		t.Fatalf("Error removing container: %v", err)
	}
}

/*
// startup & shutdown tests
n0 := non drouter network
c0 := container on n0
n1 := drouter network
c1 := container on n1
n2 := drouter network
c2 := drouter network on n2
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
