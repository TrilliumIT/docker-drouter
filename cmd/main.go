package main

import (
	"flag"
	log "github.com/Sirupsen/logrus"
	"github.com/clinta/routeShare"
	"net"
	"os"
	"os/signal"
)

var ip = flag.String("ip", "0.0.0.0", "source ip")
var port = flag.Int("port", 9999, "tcp port to listen on")
var instance = flag.Int("instance", 0, "instance number")

func main() {
	flag.Parse()
	log.Debugf("Port: %v", *port)
	sIP := net.ParseIP(*ip)

	quit := make(chan struct{})

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		close(quit)
	}()

	err := routeShare.Start(sIP, *port, *instance, quit)
	if err != nil {
		panic(err)
	}
}
