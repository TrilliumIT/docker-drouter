package main

import (
	log "github.com/Sirupsen/logrus"
	"net"
)

type subscriber struct {
	cb  chan string
	del chan struct{}
}

func startPeer() {
	var subscribers []*subscriber

	addSubscriber := make(chan *subscriber)
	delSubscriber := make(chan int)
	localRouteUpdate := make(chan string)

	listener, err := net.Listen("tcp", ":9999")
	if err != nil {
		log.Error(err)
		return
	}

	// Listen for new peers joining
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				log.Error(err)
				continue
			}
			// Handle messages with this peer
			s := &subscriber{
				cb:  make(chan string),
				del: make(chan struct{}),
			}
			addSubscriber <- s

			// die gracefully
			sendDie := make(chan struct{})
			recvDie := make(chan struct{})
			go func() {
				select {
				case _ = <-sendDie:
					close(s.del)
				case _ = <-recvDie:
					close(s.del)
				}
			}()

			// Send updates to this peer
			go func() {
				for {
					r := <-s.cb
					if r == "" { // r is closed on subscriber
						return
					}
					_, err := c.Write([]byte(r))
					if err != nil {
						log.Error(err)
						close(sendDie)
						return
					}
				}
			}()

			// Recieve messages from this peer
			go func() {
				for {
					var b []byte
					_, err := c.Read(b)
					if err != nil {
						log.Error(err)
						close(recvDie)
						return
					}
					// TODO Process external message
					log.Infof("Recieved update %v from %v", string(b), c.RemoteAddr())
				}
			}()
		}
	}()

	// Manage listening peers
	for {
		select {
		case d := <-delSubscriber:
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
