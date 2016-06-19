package main

import (
	"encoding/hex"
	"log"
	"net"
	"time"
)

const (
	srvAddr         = "224.0.0.1:9999"
	maxDatagramSize = 1300
)

func main() {
	mcastAddr, err := net.ResolveUDPAddr("udp", srvAddr)
	if err != nil {
		log.Fatal(err)
	}

	c, err := net.DialUDP("udp", nil, mcastAddr)
	go helloSender(c)
	helloListener(mcastAddr, c.LocalAddr().String(), helloReactor)
}

func helloSender(c *net.UDPConn) {
	// Send hello every second
	for {
		c.Write([]byte("hello, world\n"))
		time.Sleep(1 * time.Second)
	}
}

func helloReactor(src *net.UDPAddr, n int, b []byte) {
	log.Println(n, "bytes read from", src)
	log.Println(hex.Dump(b[:n]))
}

func helloListener(addr *net.UDPAddr, lSrc string, h func(*net.UDPAddr, int, []byte)) {
	l, err := net.ListenMulticastUDP("udp", nil, addr)
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
		if src.String() == lSrc {
			continue
		}
		go h(src, n, b)
	}
}
