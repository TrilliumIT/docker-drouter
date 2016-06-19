package main

import (
	"sync"
)

func main() {

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		startHello()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		startPeer()
	}()

	wg.Wait()
}
