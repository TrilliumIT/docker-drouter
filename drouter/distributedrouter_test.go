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

func TestRunClose(t *testing.T) {
	opts := &DistributedRouterOptions{}

	quit := make(chan struct{})
	ech := make(chan error)
	go func() {
		ech <- Run(opts, quit)
	}()

	time.Sleep(20 * time.Second)

	err := <-ech
	if err != nil {
		t.Errorf("Error on Run Return: %v", err)
	}
}
