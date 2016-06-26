package drouter

import (
	"fmt"
	"os"
	"testing"
	"time"

	dockerclient "github.com/docker/engine-api/client"
	dockerTypes "github.com/docker/engine-api/types"
	dockerCTypes "github.com/docker/engine-api/types/container"
	dockerNTypes "github.com/docker/engine-api/types/network"
	"golang.org/x/net/context"
)

var (
	dc *dockerclient.Client
	bg context.Context
)

func TestMain(m *testing.M) {
	exitStatus := 1
	defer func() { os.Exit(exitStatus) }()
	var err error

	defaultHeaders := map[string]string{"User-Agent": "engine-api-cli-1.0"}
	dc, err = dockerclient.NewClient("unix:///var/run/docker.sock", "v1.23", nil, defaultHeaders)
	if err != nil {
		return
	}
	bg = context.Background()

	exitStatus = m.Run()
}

// A most basic test to make sure it doesn't die on start
func TestRunClose(t *testing.T) {
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
	NET_NAME  = "drntest_n%v"
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
					Gateway: fmt.Sprintf(NET_IPNET, n*8+1),
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Error creating network: %v", err)
	}
	return r.ID
}

func removeNetwork(id string, t *testing.T) {
	err := dc.NetworkRemove(bg, id)
	if err != nil {
		t.Fatalf("Error removing network: %v", err)
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
