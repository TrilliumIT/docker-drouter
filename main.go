package main

import (
	"flag"
	log "github.com/Sirupsen/logrus"
	"sync"
)

var port = flag.Int("port", 9999, "tcp port to listen on")
var instance = flag.Int("instance", 0, "instance number")

func main() {
	log.SetLevel(log.DebugLevel)
	flag.Parse()
	log.Debugf("Port: %v", *port)

	var wg sync.WaitGroup

	connectPeer := make(chan string)
	defer close(connectPeer)

	hc := make(chan *hello)

	wg.Add(1)
	go func() {
		defer wg.Done()
		startHello(connectPeer, hc)
	}()

	helloMsg := <-hc
	wg.Add(1)
	go func() {
		defer wg.Done()
		startPeer(connectPeer, helloMsg)
	}()

	wg.Wait()
}
