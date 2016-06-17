package main

import (
	log "github.com/Sirupsen/logrus"
	"github.com/TrilliumIT/docker-drouter/drouter"
	"github.com/codegangsta/cli"
	"os"
	"os/signal"
	"syscall"
)

const (
	version = "0.2"
)

func main() {

	var flagDebug = cli.BoolFlag{
		Name:  "debug, d",
		Usage: "Enable debugging.",
	}
	var flagIPOffset = cli.IntFlag{
		Name:  "ip-offset",
		Value: 0,
		Usage: "",
	}
	var flagNoAggressive = cli.BoolFlag{
		Name:  "no-aggressive",
		Usage: "Set false to make drouter only connect to docker networks with local containers.",
	}
	var flagLocalShortcut = cli.BoolFlag{
		Name:  "local-shortcut",
		Usage: "Set true to insert routes in the host destined for docker networks pointing to drouter over a host<->drouter p2p link.",
	}
	var flagLocalGateway = cli.BoolFlag{
		Name:  "local-gateway",
		Usage: "Set true to insert a default route on drouter pointing to the host over the host<->drouter p2p link. (implies --local-shortcut)",
	}
	var flagMasquerade = cli.BoolFlag{
		Name:  "masquerade",
		Usage: "Set true to masquerade container traffic to it's host's interface IP address.",
	}
	var flagP2PNet = cli.StringFlag{
		Name:  "p2p-net",
		Value: "172.29.255.252/30",
		Usage: "Use this option to customize the network used for the host<->drouter p2p link.",
	}
	var flagStaticRoutes = cli.StringSliceFlag{
		Name:  "static-route",
		Usage: "Specify one or many CIDR addresses that will be installed as routes via drouter to all containers.",
	}
	var flagTransitNet = cli.StringFlag{
		Name:  "transit-net",
		Usage: "Set a transit network for drouter to always connect to. Network must have 'drouter' option set. If network has a gateway, and --local-gateway=false, drouter's default gateway will be through this network's gateway. (this option is required with --no-aggressive)",
	}
	app := cli.NewApp()
	app.Name = "docker-drouter"
	app.Usage = "Docker Distributed Router"
	app.Version = version
	app.Flags = []cli.Flag{
		flagDebug,
		flagIPOffset,
		flagNoAggressive,
		flagLocalShortcut,
		flagLocalGateway,
		flagMasquerade,
		flagP2PNet,
		flagStaticRoutes,
		flagTransitNet,
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
		FullTimestamp:    true,
	})

	if ctx.Bool("debug") {
		log.SetLevel(log.DebugLevel)
		log.Info("Debug logging enabled")
	}

	opts := &drouter.DistributedRouterOptions{
		IpOffset:      ctx.Int("ip-offset"),
		Aggressive:    !ctx.Bool("no-aggressive"),
		LocalShortcut: ctx.Bool("local-shortcut"),
		LocalGateway:  ctx.Bool("local-gateway"),
		Masquerade:    ctx.Bool("masquerade"),
		P2pNet:        ctx.String("p2p-net"),
		StaticRoutes:  ctx.StringSlice("static-route"),
		TransitNet:    ctx.String("transit-net"),
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	signal.Notify(c, syscall.SIGTERM)
	go func() {
		<-c
		err := dr.Close()
		if err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}()

	drouter.Start(opts)
	log.Debug("This space is intenionally left blank.")
}
