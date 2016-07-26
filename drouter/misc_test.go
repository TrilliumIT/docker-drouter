package drouter

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPid1(t *testing.T) {
	fmt.Println("Starting testpid1")
	if os.Getenv("TEST_NO_HOST_PID") == "" {
		return
	}

	//Get DRouter going
	quit := make(chan struct{})
	stopChan = quit

	dr, err := newDistributedRouter(defaultOpts())
	require.NoError(t, err)

	ech := make(chan error)
	go func() {
		fmt.Println("Starting DRouter.")
		ech <- dr.start()
	}()

	startDelay := time.NewTimer(10 * time.Second)
	select {
	case <-startDelay.C:
		err = nil
	case err = <-ech:
	}

	require.Error(t, err, "Run() should error if pid != host.")
	assert.Contains(t, err.Error(), "--pid=host required", "Error message should be --pid=host required")
}

func TestNoSocket(t *testing.T) {
	fmt.Println("Starting testpid1")
	if os.Getenv("TEST_NO_SOCKET") == "" {
		return
	}

	//Get DRouter going
	quit := make(chan struct{})
	stopChan = quit

	dr, err := newDistributedRouter(defaultOpts())
	require.NoError(t, err)

	ech := make(chan error)
	go func() {
		fmt.Println("Starting DRouter.")
		ech <- dr.start()
	}()

	startDelay := time.NewTimer(10 * time.Second)
	select {
	case <-startDelay.C:
		err = nil
	case err = <-ech:
	}

	require.Error(t, err, "Run() should error if no docker socket.")
	assert.Contains(t, err.Error(), "Cannot connect to the Docker daemon")
}
