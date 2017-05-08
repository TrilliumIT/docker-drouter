package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	log "github.com/Sirupsen/logrus"
	"github.com/TrilliumIT/docker-drouter/drouter"
	"github.com/urfave/cli"
)

const (
	version = "0.3"
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
	var flagDRouterInstance = cli.StringFlag{
		Name:  "drouter-instance",
		Value: "drouter",
		Usage: "String to tell this drouter instance which networks to connect to. (must match 'docker network create -o drouter=<instance>')",
	}
	var flagNoAggressive = cli.BoolFlag{
		Name:  "no-aggressive",
		Usage: "Set false to make drouter only connect to docker networks with local containers.",
	}
	var flagHostShortcut = cli.BoolFlag{
		Name:  "host-shortcut",
		Usage: "Set true to insert routes in the host destined for docker networks pointing to drouter over a host<->drouter p2p link.",
	}
	var flagContainerGateway = cli.BoolFlag{
		Name:  "container-gateway",
		Usage: "Set true to set the container gateway to drouter.",
	}
	var flagHostGateway = cli.BoolFlag{
		Name:  "host-gateway",
		Usage: "Set true to insert a default route on drouter pointing to the host over the host<->drouter p2p link. (implies --host-shortcut)",
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
		Usage: "Set a transit network for drouter to always connect to. Network should have 'drouter' option set. If network has a gateway, and --host-gateway=false, drouter's default gateway will be through this network's gateway. (this option is required with --no-aggressive)",
	}
	var flagLocalRoutePriority = cli.IntFlag{
		Name:  "local-route-priority",
		Value: 10,
		Usage: "Increase the metric for networks that this drouter instance is directly connected to.",
	}
	var flagRemoteRoutePriority = cli.IntFlag{
		Name:  "remote-route-priority",
		Value: 100,
		Usage: "Increase the metric for any routes which are provided by other drouters.",
	}
	var flagDrouterPort = cli.IntFlag{
		Name:  "drouter-port",
		Value: 9999,
		Usage: "The port that drouter will listen on for connections to other drouters",
	}

	app := cli.NewApp()
	app.Name = "docker-drouter"
	app.Usage = "Docker Distributed Router"
	app.Version = version
	app.Flags = []cli.Flag{
		flagDebug,
		flagIPOffset,
		flagDRouterInstance,
		flagNoAggressive,
		flagHostShortcut,
		flagContainerGateway,
		flagHostGateway,
		flagMasquerade,
		flagP2PNet,
		flagStaticRoutes,
		flagTransitNet,
		flagRemoteRoutePriority,
		flagLocalRoutePriority,
		flagDrouterPort,
	}
	app.Action = Run
	err := app.Run(os.Args)
	if err != nil {
		log.WithError(err).Fatal("Error from Run")
	}
}

// Run initializes the driver
func Run(ctx *cli.Context) error {
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
	//if ctx.Bool("no-aggressive") {
	//	log.Error("Sorry, --no-aggressive mode is not supported yet.")
	//	return fmt.Errorf("Unsupported Options")
	//}
	if ctx.Bool("host-shortcut") {
		log.Error("Sorry, --host-shortcut mode is not supported yet.")
		return fmt.Errorf("Unsupported Options")
	}
	if ctx.Bool("container-gateway") {
		log.Error("Sorry, --container-gateway mode is not supported yet.")
		return fmt.Errorf("Unsupported Options")
	}
	if ctx.Bool("host-gateway") {
		log.Error("Sorry, --host-gateway mode is not supported yet.")
		return fmt.Errorf("Unsupported Options")
	}
	if ctx.Bool("masquerade") {
		log.Error("Sorry, --masquerade mode is not supported yet.")
		return fmt.Errorf("Unsupported Options")
	}
	//if ctx.String("transit-net") != "" {
	//	log.Error("Sorry, --transit-net is not supported yet.")
	//	return fmt.Errorf("Unsupported Options")
	//}

	opts := &drouter.DistributedRouterOptions{
		IPOffset:            ctx.Int("ip-offset"),
		Aggressive:          !ctx.Bool("no-aggressive"),
		HostShortcut:        ctx.Bool("host-shortcut"),
		ContainerGateway:    ctx.Bool("container-gateway"),
		HostGateway:         ctx.Bool("host-gateway"),
		Masquerade:          ctx.Bool("masquerade"),
		P2PAddr:             ctx.String("p2p-net"),
		StaticRoutes:        ctx.StringSlice("static-route"),
		TransitNet:          ctx.String("transit-net"),
		InstanceName:        ctx.String("drouter-instance"),
		LocalRoutePriority:  ctx.Int("local-route-priority"),
		RemoteRoutePriority: ctx.Int("remote-route-priority"),
		DrouterPort:         ctx.Int("drouter-port"),
	}

	quit := make(chan struct{})

	c := make(chan os.Signal)
	defer close(c)
	signal.Notify(c, os.Interrupt)
	signal.Notify(c, syscall.SIGTERM)
	go func() {
		<-c
		close(quit)
	}()

	err := drouter.Run(opts, quit)
	if err != nil {
		log.WithFields(log.Fields{"Error": err}).Error("Error running drouter.")
		return err
	}

	return nil
}
