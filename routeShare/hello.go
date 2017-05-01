package routeShare

import (
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
)

type hello struct {
	ListenAddr string
	Instance   int
}

func (r *RouteShare) startHello(connectPeer chan<- string, hc chan<- *hello, done <-chan struct{}) error {
	mcastAddr, err := net.ResolveUDPAddr("udp", "224.0.0.1:9999")
	if err != nil {
		return err
	}

	c, err := net.DialUDP("udp", &net.UDPAddr{IP: r.ip}, mcastAddr)
	if err != nil {
		return err
	}

	lAddr, _, err := net.SplitHostPort(c.LocalAddr().String())
	if err != nil {
		return err
	}
	helloMsg := &hello{
		ListenAddr: lAddr + ":" + strconv.Itoa(r.port),
		Instance:   r.instance,
	}

	hc <- helloMsg
	for len(hc) > 0 {
	}
	close(hc)

	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	// Send hello packets every second
	go func(t *time.Ticker) {
		e := json.NewEncoder(c)
		for range t.C {
			err2 := e.Encode(helloMsg)
			if err2 != nil {
				log.WithError(err).Error("Failed to encode hello")
				continue
			}
		}
	}(t)

	l, err := net.ListenMulticastUDP("udp", nil, mcastAddr)
	if err != nil {
		return err
	}
	go func() {
		<-done
		err := l.Close()
		if err != nil {
			log.WithError(err).Error("Error closing multicast listener")
		}
	}()

	d := json.NewDecoder(l)
	for {
		h := &hello{}
		err := d.Decode(h)
		if err != nil {
			if strings.HasSuffix(err.Error(), "use of closed network connection") {
				return nil
			}
			log.WithError(err).Error("Unable to decode hello")
			continue
		}

		if h.Instance != helloMsg.Instance {
			continue
		}
		if h.ListenAddr == helloMsg.ListenAddr {
			continue
		}
		connectPeer <- h.ListenAddr
	}
}
