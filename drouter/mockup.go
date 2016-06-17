package drouter

import (
	"github.com/docker/engine-api/types/events"
	"github.com/vishvananda/netlink"
)

var ()

func mainLoop() {
	aggressiveTick := make(chan string)
	dockerEvent := make(chan *events.Message)
	routeSub := make(chan *netlink.RouteUpdate)
	shutdown := make(chan string)

	select {
	case _ <- aggressiveTick:
		scanNetworks()

	case e <- dockerEvent:
		processEvent(e)

	case r <- routeSub:
		processRoute(r)

	case _ <- shutdown:
		shutdown()
	}
}
