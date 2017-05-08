package routeShare

import (
	"net"

	"sync"
	"syscall"

	log "github.com/Sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

// RouteShare is the object which holds the sharing daemon. It quits when the quit channel is closed
type RouteShare struct {
	ip               net.IP
	port             int
	instance         string
	quit             <-chan struct{}
	localRouteUpdate chan *exportRoute
	priority         int
}

// NewRouteShare returns a new RouteShare object
func NewRouteShare(ip net.IP, port int, instance string, priority int, quit <-chan struct{}) *RouteShare {
	return &RouteShare{
		ip:               ip,
		port:             port,
		instance:         instance,
		quit:             quit,
		localRouteUpdate: make(chan *exportRoute),
		priority:         priority,
	}
}

// Start starts the RouteShare daemon. Blocking until quit is closed.
func (r *RouteShare) Start() error {
	var wg sync.WaitGroup

	connectPeer := make(chan string)
	defer close(connectPeer)

	hc := make(chan *hello)

	ech := make(chan error)
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		ech <- r.startHello(connectPeer, hc, done)
	}()

	helloMsg := <-hc
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.startPeer(connectPeer, helloMsg, done)
	}()

	var err error
	select {
	case err = <-ech:
		close(done)
	case <-r.quit:
		close(done)
		err = <-ech
	}

	wg.Wait()
	return err
}

const (
	// AddRoute signals a route has been added and should be published
	AddRoute = true
	// DelRoute signals a route has been deleted and should be unpublished
	DelRoute = false
)

// ModifyRoute modifies a published route
func (r *RouteShare) ModifyRoute(dst *net.IPNet, action bool) {
	if !isDirect(dst.IP) {
		log.WithField("dst", dst).Debug("Refusing to publish non-direct route")
		return
	}
	er := &exportRoute{
		Dst: dst,
	}
	rl := log.WithField("update", er).WithField("dst", er.Dst)
	if er.Dst == nil || er.Dst.IP == nil || er.Dst.IP.Equal(net.IP{0, 0, 0, 0}) {
		log.Debug("Refusing to publish route to nil")
		return
	}
	if action {
		er.Type = syscall.RTM_NEWROUTE
		rl.Debug("Sending route add")
	}
	if !action {
		er.Type = syscall.RTM_DELROUTE
		log.WithField("update", er).Debug("Sending route delete")
		rl.Debug("Sending route delete")
	}
	r.localRouteUpdate <- er
}

// AddRoute adds a published route
func (r *RouteShare) AddRoute(dst *net.IPNet) {
	r.ModifyRoute(dst, AddRoute)
}

// DelRoute deletes a published route
func (r *RouteShare) DelRoute(dst *net.IPNet) {
	r.ModifyRoute(dst, DelRoute)
}

// ShareRouteUpdate publishes a route update message
func (r *RouteShare) ShareRouteUpdate(ru *netlink.RouteUpdate) {
	if ru.Gw != nil {
		// we only care about directly connected routes
		return
	}
	if ru.Table == 255 {
		// We don't want entries from the local routing table
		// http://linux-ip.net/html/routing-tables.html
		return
	}
	if ru.Src.IsLoopback() {
		return
	}
	if ru.Dst.IP.IsLoopback() {
		return
	}
	if ru.Src.IsLinkLocalUnicast() {
		return
	}
	if ru.Dst.IP.IsLinkLocalUnicast() {
		return
	}
	if ru.Dst.IP.IsInterfaceLocalMulticast() {
		return
	}
	if ru.Dst.IP.IsLinkLocalMulticast() {
		return
	}
	er := &exportRoute{
		Type:     ru.Type,
		Dst:      ru.Dst,
		Gw:       ru.Gw,
		Priority: ru.Priority,
	}
	log.Debugf("Sending route update: %v", er)
	r.localRouteUpdate <- er
}
