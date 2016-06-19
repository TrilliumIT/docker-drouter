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

func startHello(connectPeer chan<- string, hc chan<- []byte) {
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
	msg := hello{
		ListenAddr: lAddr + ":" + strconv.Itoa(*port),
		Instance:   *instance,
	}

	helloMsg := make([]byte, 512)
	helloMsg, err = json.Marshal(msg)
	if err != nil {
		log.Error("Unable to marshall hello")
		log.Fatal(err)
	}

	hc <- helloMsg
	for len(hc) > 0 {
	}
	close(hc)

	log.Debugf("Json: %v", string(helloMsg))

	// Send hello packets every second
	go func() {
		for {
			c.Write(helloMsg)
			time.Sleep(1 * time.Second)
		}
	}()

	l, err := net.ListenMulticastUDP("udp", nil, mcastAddr)
	if err != nil {
		log.Fatal(err)
	}

	l.SetReadBuffer(512)

	for {
		b := make([]byte, 512)
		_, src, err := l.ReadFromUDP(b)
		if err != nil {
			log.Fatal("ReadFromUDP failed:", err)
			continue
		}

		h := &hello{}
		err = json.Unmarshal(b, h)
		if err != nil {
			log.Error("Unable to unmarshall hello")
			log.Error(err)
			continue
		}

		if h.Instance != msg.Instance {
			continue
		}
		if h.ListenAddr == msg.ListenAddr {
			continue
		}

		log.Debugf("%v recieved from %v", string(b), src)
		connectPeer <- h.ListenAddr

		log.Debugf("%v recieved from %v", string(b), src)
		connectPeer <- h.ListenAddr
	}

}
