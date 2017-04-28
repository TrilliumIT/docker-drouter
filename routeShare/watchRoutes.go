package routeShare

import (
	log "github.com/Sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

func watchRoutes(localRouteUpdate chan<- *exportRoute, done <-chan struct{}) {
	ruc := make(chan netlink.RouteUpdate)
	err := netlink.RouteSubscribe(ruc, done)
	go func() {
		<-done
	}()
	if err != nil {
		log.Error("Error subscribing to route table")
		log.Fatal(err)
	}
	for ru := range ruc {
		if ru.Gw != nil {
			// we only care about directly connected routes
			continue
		}
		if ru.Table == 255 {
			// We don't want entries from the local routing table
			// http://linux-ip.net/html/routing-tables.html
			continue
		}
		if ru.Src.IsLoopback() {
			continue
		}
		if ru.Dst.IP.IsLoopback() {
			continue
		}
		if ru.Src.IsLinkLocalUnicast() {
			continue
		}
		if ru.Dst.IP.IsLinkLocalUnicast() {
			continue
		}
		if ru.Dst.IP.IsInterfaceLocalMulticast() {
			continue
		}
		if ru.Dst.IP.IsLinkLocalMulticast() {
			continue
		}
		er := &exportRoute{
			Type:     ru.Type,
			Dst:      ru.Dst,
			Gw:       ru.Gw,
			Priority: ru.Priority,
		}
		log.Debugf("Sending route update: %v", er)
		localRouteUpdate <- er
	}
}
