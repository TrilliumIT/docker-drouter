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
	/*
		Create a channel that the UDP sender will use to notify the
		UDP reciever of what the source address is.
		That way the UDP reciever can know to ignore packets
		sent by this local sender.
	*/
	helloSrcChan := make(chan string)
	go helloSender(srvAddr, helloSrcChan)
	helloListener(srvAddr, helloReactor, helloSrcChan)
}

func helloSender(a string, sc chan<- string) {
	addr, err := net.ResolveUDPAddr("udp", a)
	if err != nil {
		log.Fatal(err)
	}
	c, err := net.DialUDP("udp", nil, addr)
	// Send the source address to the reciever routine
	sc <- c.LocalAddr().String()
	// Block until the reciever has read the source
	// Tested, this does not busy CPU
	for len(sc) > 0 {
	}
	close(sc)

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

func helloListener(a string, h func(*net.UDPAddr, int, []byte), sc <-chan string) {
	addr, err := net.ResolveUDPAddr("udp", a)
	if err != nil {
		log.Fatal(err)
	}
	l, err := net.ListenMulticastUDP("udp", nil, addr)
	l.SetReadBuffer(maxDatagramSize)

	// Get the source addres used by sender, so we can ignore it.
	// Includes source port
	lSrc := <-sc
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
