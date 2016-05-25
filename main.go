package main

import (
	"os"

	log "github.com/Sirupsen/logrus"
	"github.com/clinta/docker-drouter/drouter"
	"github.com/docker/go-plugins-helpers/network"
	"github.com/codegangsta/cli"
)

const (
	version = "0.6"
)

func main() {

	var flagDebug = cli.BoolFlag{
		Name:  "debug, d",
		Usage: "Enable debugging.",
	}
	var flagUseGatewayIP = cli.BoolFlag{
		Name:  "use-gateway-ip",
		Usage: "Use the gateway IP when creating the router interface. If this is not specified, the routing container will be assigned an address by IPAM.",
	}
	var flagAggressive = cli.BoolFlag{
		Name:  "aggressive",
		Usage: "Scan for new networks and create routing interfaces for all docker networks with the drouter option set, regardless of whether or not there are any containers on that network on this host.",
	}
	var flagDisableP2P = cli.BoolFlag{
		Name:  "disable-p2p",
		Usage: "Disable the creation of a p2p link between the host and the routing container. Use this option if you do not wish you want traffic routed between container networks but not between the host and the container",
	}
	var flagP2PAddr = cli.StringFlag{
		name: "p2p-addr",
		Value: "172.29.255.252/30"
		Usage: "The network to use for routing between the host and the container. The host will be assigned the first host address in the network, the container will be assigned the second. This is a p2p link so anything beyond a /30 is unnecessary",
	}
	app := cli.NewApp()
	app.Name = "docker-drouter"
	app.Usage = "Docker Distributed Router"
	app.Version = version
	app.Flags = []cli.Flag{
		flagDebug,
		flagUseGatewayIP,
		flagAggressive,
	}
	app.Action = Run
	app.Run(os.Args)
}

// Run initializes the driver
func Run(ctx *cli.Context) {
	if ctx.Bool("debug") {
		log.SetLevel(log.DebugLevel)
	}
	log.SetFormatter(&log.TextFormatter{
		ForceColors: false,
		DisableColors: true,
		DisableTimestamp: false,
		FullTimestamp: true,
	})

	if !ctx.Bool("disable-p2p") {
		err := drouter.MakeP2PLink(ctx.String("p2p-addr"))
		if err != nil {
			log.Error("Error creating P2P Link")
			log.Fatal(err)
		}
	}
	
	if ctx.Bool("aggressive") {
		go drouter.WatchNetworks()
	}

	go drouter.WatchEvents()

}
