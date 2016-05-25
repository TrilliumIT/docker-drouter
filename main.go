package main

import (
	"os"

	log "github.com/Sirupsen/logrus"
	"github.com/clinta/docker-drouter/mvrouter"
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
		Usage: "Create routing interfaces for all docker networks with the macvlan-router option set, regardless of whether or not there are any containers on that network on this host.",
	}
	app := cli.NewApp()
	app.Name = "docker-drouter"
	app.Usage = "Docker Distributed Router"
	app.Version = version
	app.Flags = []cli.Flag{
		flagDebug,
		flagScope,
		flagVtepDev,
		flagAllowEmpty,
		flagLocalGateway,
		flagGlobalGateway,
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

	if ctx.Bool("local-gateway") && ctx.Bool("global-gateway") {
		panic("local-gateway and global-gateway cannot both be enabled on the same host")
	}

	d, err := vxlan.NewDriver(ctx.String("scope"), ctx.String("vtepdev"), ctx.Bool("allow-empty"), ctx.Bool("local-gateway"), ctx.Bool("global-gateway"))
	if err != nil {
		panic(err)
	}
	h := network.NewHandler(d)
	h.ServeUnix("root", "vxlan")
}
