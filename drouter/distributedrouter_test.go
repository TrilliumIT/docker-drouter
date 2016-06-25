package drouter

import (
	dockerclient "github.com/docker/engine-api/client"
	"os"
	"testing"
	"time"
	//dockertypes "github.com/docker/engine-api/types"
	//dockerevents "github.com/docker/engine-api/types/events"
	//dockerfilters "github.com/docker/engine-api/types/filters"
)

var (
	dc *dockerclient.Client
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

func newContainer() (*container, error) {
}

/*
every mode needs to do the following:
n1 := non drouter network
c1 := container on n1
n2 := drouter network
c2 := container on c2
n3 := drouter network
c3 := drouter network on n3
c23 := container on network n2 and n3
n4 := drouter network

start drouter

//test networks:
-not connected to n1
-connected to n2
-connected to n3
if aggressive:
	-connected to n4
else:
	-not connected to n4

// test containers:
-c1 routes unchanged
if !containerGateway:
	-c2 contains route to n3
	-c3 contains route to n2
	-c23 routes unchanged
else:
	-c2 gateway changed
	-c3 gateway changed
	-c23 gateway is one of the correct options



*/
