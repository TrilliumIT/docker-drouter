package drouter

import (
	dockerclient "github.com/docker/engine-api/client"
	"golang.org/x/net/context"
	"os"
	"testing"
	"time"
	//dockertypes "github.com/docker/engine-api/types"
	//dockerevents "github.com/docker/engine-api/types/events"
	//dockerfilters "github.com/docker/engine-api/types/filters"
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

func createNetwork(n int) {
	name := fmt.Stringf("n%v", n)
	dc.NetworkCreate(bg, name, dockertypes.NetworkCreate{
		Options: make(map[string]string{"drouter": "true"}),
		IPAM: dockerNetworkTypes.IPAM{
			Config: []dockerNetworkTypes.IPAMConfig{
				dockerNetworkTypes.IPAMConfig{
					Subnet:  fmt.Stringf("192.168.242.%v/29", n*8),
					Gateway: fmt.Stringf("192.168.242.%v/29", n*8+1),
				},
			},
		},
	})
}

var n []*network
var c []*container

func newNetwork()

func newContainer() (*container, error) {
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
