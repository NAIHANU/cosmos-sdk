package listener

import "context"

func Async(listener Listener, bufferSize int, commitChan chan<- error) Listener {
	packetChan := make(chan Packet, bufferSize)
	res := PacketCollector(func(p Packet) error {
		packetChan <- p
		return nil
	})

	var cancel <-chan struct{}
	res.Initialize = func(ctx context.Context, data InitializationData) (lastBlockPersisted int64, err error) {
		cancel = ctx.Done()
		if listener.Initialize != nil {
			return listener.Initialize(ctx, data)
		}
		return 0, nil
	}
	res.InitializeModuleSchema = listener.InitializeModuleSchema

	var err error
	res.CompleteInitialization = func() error {
		go func() {
			for {
				select {
				case packet := <-packetChan:
					if err != nil {
						// if we have an error, don't process any more packets
						// and return the error and finish when it's time to commit
						if _, ok := packet.(Commit); ok {
							commitChan <- err
							return
						}
					} else {
						// process the packet
						err = listener.SendPacket(packet)
						// if it's a commit
						if _, ok := packet.(Commit); ok {
							commitChan <- err
							if err != nil {
								return
							}
						}
					}

				case <-cancel:
					return
				}
			}
		}()
		return nil
	}

	return res
}

func AsyncMultiplex(listeners []Listener, bufferSize int) Listener {
	asyncListeners := make([]Listener, len(listeners))
	commitChans := make([]chan error, len(listeners))
	for i, l := range listeners {
		commitChan := make(chan error)
		commitChans[i] = commitChan
		asyncListeners[i] = Async(l, bufferSize, commitChan)
	}
	mux := Multiplex(asyncListeners...)
	muxCommit := mux.Commit
	mux.Commit = func() error {
		err := muxCommit()
		if err != nil {
			return err
		}

		for _, commitChan := range commitChans {
			err := <-commitChan
			if err != nil {
				return err
			}
		}
		return nil
	}

	return mux
}
