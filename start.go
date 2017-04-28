package routeShare

import (
	log "github.com/Sirupsen/logrus"
	"net"
	"sync"
)

func Start(ip net.IP, port, instance int, quit <-chan struct{}) error {
	log.SetLevel(log.DebugLevel)

	var wg sync.WaitGroup

	connectPeer := make(chan string)
	defer close(connectPeer)

	hc := make(chan *hello)

	ech := make(chan error)
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		ech <- startHello(ip, port, instance, connectPeer, hc, done)
	}()

	helloMsg := <-hc
	wg.Add(1)
	go func() {
		defer wg.Done()
		startPeer(connectPeer, helloMsg, done)
	}()

	var err error
	select {
	case err = <-ech:
		close(done)
	case <-quit:
		close(done)
		err = <-ech
	}

	wg.Wait()
	return err
}
