package main

import (
	"os"
	"os/signal"
	"syscall"

	log "github.com/Sirupsen/logrus"
  "./drouter"
	"github.com/codegangsta/cli"
)

const (
	version = "0.1"
)

func main() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	signal.Notify(c, syscall.SIGTERM)
	go func() {
		<-c
		err := drouter.Cleanup()
		if err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}()

	var flagDebug = cli.BoolFlag{
		Name:  "debug, d",
		Usage: "Enable debugging.",
	}
	var flagAggressive = cli.BoolFlag{
		Name:  "aggressive",
		Value: true,
		Usage: "Scan for new networks and create routing interfaces for all docker networks with the drouter option set, regardless of whether or not there are any containers on that network on this host. (Currently enabled by default as passive mode is not yet implemented)",
	}
  var flagSummaryNets = cli.StringSliceFlag{
		Name: "summary-net",
		Usage: "Each instance of this option will be injected into each containers routing table once. It is expected that the summary networks cover ONLY and ALL of your vxlan networks. The default behavior adds the prefix for every vxlan into every containers routing table as vxlans are created, this option adds the routs only at container creation time.",
	}
  var flagHostGatway = cli.BoolFlat{
		Name: "host-gateway",
		Value: false,
		Usage: "Set to true to allow containers to use the host's default route.",
	}
	var flagMasquerade = cli.BoolFlag{
		Name: "masquerade",
		Value: false,
		Usage: "When using the host-gateway option, masquerade traffic to the host's interface address.",
	}
	var flagP2PAddr = cli.StringFlag{
		Name: "p2p-addr",
		Value: "172.29.255.252/30",
		Usage: "When using the host-gateway option, use this p2p prefix for routing between the host and the drouter container. The host will be assigned the first host address in the network, the container will be assigned the second. This is a p2p link so anything beyond a /30 is unnecessary",
	}
	app := cli.NewApp()
	app.Name = "docker-drouter"
	app.Usage = "Docker Distributed Router"
	app.Version = version
	app.Flags = []cli.Flag{
		flagDebug,
		flagAggressive,
		flagSummaryNets,
		flagHostGateway,
		flagMasquerade,
		flagP2PAddr,
	}
	app.Action = Run
	app.Run(os.Args)
}

// Run initializes the driver
func Run(ctx *cli.Context) {
	log.SetFormatter(&log.TextFormatter{
		//ForceColors: false,
		//DisableColors: true,
		DisableTimestamp: false,
		FullTimestamp: true,
	})

	if ctx.Bool("debug") {
		log.SetLevel(log.DebugLevel)
		log.Info("Debug logging enabled")
	}

  dr, err := NewDistributedRouter(&drouter.DistrubutedRouterOptions{
		ipOffset: ctx.Int("ip-offset"),
		aggressive: ctx.Bool("aggressive"),
		summaryNets: ctx.StringSlice("summary-net"),
		hostGateway: ctx.Bool("host-gateway"),
		masquerade: ctx.Bool("masquerade"),
		p2pAddr: ctx.String("p2p-addr"),
	})

	if err != nil {
		log.Fatal(err)
	}

	defer dr.Close()
	dr.Start()
}
