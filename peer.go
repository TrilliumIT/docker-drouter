package main

import (
	"encoding/gob"
	log "github.com/Sirupsen/logrus"
	"net"
	"strconv"
)

type subscriber struct {
	cb  chan *exportRoute
	del chan struct{}
}

func startTCPListener(peerCh chan<- *net.Conn) {
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(*port))
	if err != nil {
		log.Error("Failed to setup listener")
		log.Error(err)
		return
	}

	for {
		c, err := listener.Accept()
		if err != nil {
			log.Error("Failed to accept listener")
			log.Error(err)
			continue
		}
		peerCh <- &c
	}
}

type procConReqOpts struct {
	peerCh         chan<- *net.Conn
	connectPeer    <-chan string
	peerConnected  <-chan string
	disconnectPeer <-chan string
}

func processConnectionRequest(o *procConReqOpts) {
	peers := make(map[string]struct{})
	for {
		select {
		case p := <-o.connectPeer:
			if _, ok := peers[p]; !ok {
				c, err := net.Dial("tcp", p)
				if err != nil {
					log.Error("Failed to dial")
					log.Error(err)
					continue
				}
				o.peerCh <- &c
			}
		case p := <-o.peerConnected:
			peers[p] = struct{}{}
			log.Debugf("Current peers: %v", peers)
		case p := <-o.disconnectPeer:
			delete(peers, p)
			log.Debugf("Current peers: %v", peers)
		}
	}
}

func startPeer(connectPeer <-chan string, helloMsg *hello) {
	localRouteUpdate := make(chan *exportRoute)

	// Monitor local route table changes
	go watchRoutes(localRouteUpdate)

	// listen for incoming tcp connections
	peerCh := make(chan *net.Conn)
	go startTCPListener(peerCh)

	// process new connections, from hello or tcp
	peerConnected := make(chan string)
	disconnectPeer := make(chan string)
	procOpts := &procConReqOpts{
		peerCh:         peerCh,
		connectPeer:    connectPeer,
		peerConnected:  peerConnected,
		disconnectPeer: disconnectPeer,
	}
	go processConnectionRequest(procOpts)

	// Handle new peers joining
	var subscribers []*subscriber
	addSubscriber := make(chan *subscriber)

	peerOpts := &peerConnOpts{
		helloMsg:       helloMsg,
		peerCh:         peerCh,
		peerConnected:  peerConnected,
		addSubscriber:  addSubscriber,
		disconnectPeer: disconnectPeer,
	}
	go peerConnections(peerOpts)

	// Manage listening peers
	delSubscriber := make(chan int)
	for {
		select {
		case d := <-delSubscriber:
			if len(subscribers) <= 1 {
				subscribers = []*subscriber{}
				continue
			}
			subscribers = append(subscribers[:d], subscribers[d+1:]...)
		case s := <-addSubscriber:
			i := len(subscribers)
			subscribers = append(subscribers, s)
			// Start a routine to listen for closing s.del
			// send delSubscriber for the position of s when triggered
			go func() {
				_ = <-s.del
				close(s.cb)
				delSubscriber <- i
			}()
		case r := <-localRouteUpdate:
			for _, s := range subscribers {
				select {
				// just in case a del is pending
				case _ = <-s.del:
					break
				default:
					s.cb <- r
				}
			}
		}
	}
}

type peerConnOpts struct {
	helloMsg       *hello
	peerCh         <-chan *net.Conn
	peerConnected  chan<- string
	addSubscriber  chan<- *subscriber
	disconnectPeer chan<- string
}

func peerConnections(o *peerConnOpts) {
	for {
		c := *<-o.peerCh

		// Send hello on all new tcp connections
		e := gob.NewEncoder(c)
		err := e.Encode(o.helloMsg)
		if err != nil {
			log.Error("Failed to encode tcp hello on connect")
			log.Error(err)
			return
		}

		// get corresponding hello from the other side
		d := gob.NewDecoder(c)
		h := &hello{}
		err = d.Decode(h)
		if err != nil {
			log.Error("Failed to decode tcp hello on connect")
			log.Error(err)
			return
		}

		log.Debugf("hello recieved via tcp: %v", h)
		o.peerConnected <- h.ListenAddr

		// Handle messages with this peer
		s := &subscriber{
			cb:  make(chan *exportRoute),
			del: make(chan struct{}),
		}
		o.addSubscriber <- s

		// die gracefully
		sendDie := make(chan struct{})
		recvDie := make(chan struct{})
		go func() {
			select {
			case _ = <-sendDie:
				break
			case _ = <-recvDie:
				break
			}
			close(s.del)
			o.disconnectPeer <- c.RemoteAddr().String()
			err := delAllRoutesVia(c.RemoteAddr())
			if err != nil {
				log.Errorf("Failed to delete all routes via %v", c.RemoteAddr())
			}
		}()

		// Send updates to this peer
		go func() {
			e := gob.NewEncoder(c)
			srcAddr := c.LocalAddr().(*net.TCPAddr).IP
			for {
				r := <-s.cb
				if r == nil { // r is closed on subscriber
					return
				}
				if (r.Dst.IP.To4() == nil) != (srcAddr.To4() == nil) {
					// Families don't match
					continue
				}
				err := e.Encode(r)
				if err != nil {
					log.Error("Failed to encode exportRoute")
					log.Error(err)
					close(sendDie)
					return
				}
			}
		}()

		// Recieve messages from this peer
		go func() {
			d := gob.NewDecoder(c)
			for {
				er := &exportRoute{}
				err := d.Decode(er)
				if err != nil {
					log.Error("Failed to decode route update")
					log.Error(err)
					close(recvDie)
					return
				}

				log.Debugf("Recieved update %v from %v", *er, c.RemoteAddr())
				err = processRoute(er, c.RemoteAddr())
				if err != nil {
					log.Errorf("Failed to process route: %v", *er)
					continue
				}
			}
		}()
	}
}
