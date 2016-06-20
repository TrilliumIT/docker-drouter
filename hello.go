package main

import (
	"encoding/json"
	log "github.com/Sirupsen/logrus"
	"net"
	"strconv"
	"time"
)

type hello struct {
	ListenAddr string
	Instance   int
}

func startHello(connectPeer chan<- string, hc chan<- *hello) {
	mcastAddr, err := net.ResolveUDPAddr("udp", "224.0.0.1:9999")
	if err != nil {
		log.Fatal(err)
	}

	c, err := net.DialUDP("udp", nil, mcastAddr)

	lAddr, _, err := net.SplitHostPort(c.LocalAddr().String())
	if err != nil {
		log.Error("Failed to split host port")
		log.Fatal(err)
	}
	helloMsg := &hello{
		ListenAddr: lAddr + ":" + strconv.Itoa(*port),
		Instance:   *instance,
	}

	hc <- helloMsg
	for len(hc) > 0 {
	}
	close(hc)

	// Send hello packets every second
	go func() {
		e := json.NewEncoder(c)
		for {
			err := e.Encode(helloMsg)
			if err != nil {
				log.Error("Failed to encode hello")
				log.Error(err)
				continue
			}
			time.Sleep(1 * time.Second)
		}
	}()

	l, err := net.ListenMulticastUDP("udp", nil, mcastAddr)
	if err != nil {
		log.Fatal(err)
	}

	d := json.NewDecoder(l)
	for {
		h := &hello{}
		err := d.Decode(h)
		if err != nil {
			log.Error("Unable to decode hello")
			log.Error(err)
			continue
		}

		if h.Instance != helloMsg.Instance {
			continue
		}
		if h.ListenAddr == helloMsg.ListenAddr {
			continue
		}
		connectPeer <- h.ListenAddr
	}

}
