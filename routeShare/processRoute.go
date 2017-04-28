package routeShare

import (
	log "github.com/Sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"net"
	"syscall"
)

type routeUpdate struct {
	src net.Addr
	msg []byte
}

func delAllRoutesVia(s net.Addr) error {
	src := s.(*net.TCPAddr).IP

	routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		log.Error("Failed to get routes")
		log.Error(err)
		return err
	}
	for _, r := range routes {
		if r.Gw.Equal(src) {
			netlink.RouteDel(&r)
		}
	}
	return nil
}

type exportRoute struct {
	Type     uint16
	Dst      *net.IPNet
	Gw       net.IP
	Priority int
}

func processRoute(ru *exportRoute, s net.Addr) error {
	src := s.(*net.TCPAddr).IP

	_, err := netlink.AddrList(nil, netlink.FAMILY_ALL)
	if err != nil {
		log.Error("Failed to get addresses")
		log.Error(err)
		return err
	}

	switch {
	case ru.Type == syscall.RTM_NEWROUTE:
		err := netlink.RouteAdd(&netlink.Route{
			Dst:      ru.Dst,
			Gw:       src,
			Priority: ru.Priority + 100,
		})
		if err != nil {
			log.WithField("update", ru).WithField("source", src).WithError(err).Error("Error adding route")
			return err
		}
	case ru.Type == syscall.RTM_DELROUTE:
		err := netlink.RouteDel(&netlink.Route{
			Dst:      ru.Dst,
			Gw:       src,
			Priority: ru.Priority + 100,
		})
		if err != nil {
			if err.Error() == "no such process" {
				// The route doesn't exist, no problem
				return nil
			}
			log.WithField("update", ru).WithField("source", src).WithError(err).Error("Error deleting route")
			return err
		}
	}
	return nil
}
