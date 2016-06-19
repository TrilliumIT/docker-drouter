package main

import (
	"encoding/hex"
	log "github.com/Sirupsen/logrus"
	"net"
	"time"
)

func startHello() {
	helloMsg := []byte("routeShare")
	maxDatagramSize := len(helloMsg)

	mcastAddr, err := net.ResolveUDPAddr("udp", "224.0.0.1:9999")
	if err != nil {
		log.Fatal(err)
	}

	c, err := net.DialUDP("udp", nil, mcastAddr)

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

	l.SetReadBuffer(maxDatagramSize)

	for {
		b := make([]byte, maxDatagramSize)
		n, src, err := l.ReadFromUDP(b)
		if err != nil {
			log.Fatal("ReadFromUDP failed:", err)
			continue
		}
		if string(b) != string(helloMsg) {
			continue
		}
		if src.String() == c.LocalAddr().String() {
			continue
		}

		log.Println(n, "bytes read from", src)
		log.Println(hex.Dump(b[:n]))
	}

}
