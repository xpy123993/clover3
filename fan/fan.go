package fan

import (
	"context"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// RedirectionConfig specifies the configuration of stateless fan-in forwarding.
type RedirectionConfig struct {
	// The timeout of a connection in idle state.
	Timeout time.Duration
	// FanIn function will allocate buffer in `BufferSize` while listening. A too large size might cause OOMing.
	BufferSize int64
}

type channelWrapper struct {
	mu         sync.Mutex
	connection net.Conn
	lastActive time.Time
	isclosed   bool
}

func (channel *channelWrapper) isClosed() bool {
	channel.mu.Lock()
	defer channel.mu.Unlock()
	return channel.isclosed
}

func (channel *channelWrapper) refreshActiveTimestamp() {
	channel.mu.Lock()
	defer channel.mu.Unlock()
	channel.lastActive = time.Now()
}

func (channel *channelWrapper) write(data []byte) (int, error) {
	channel.refreshActiveTimestamp()
	return channel.connection.Write(data)
}

func (channel *channelWrapper) outdated(timeout time.Duration) bool {
	channel.mu.Lock()
	defer channel.mu.Unlock()
	return channel.lastActive.Add(timeout).Before(time.Now())
}

func (channel *channelWrapper) close() {
	channel.mu.Lock()
	defer channel.mu.Unlock()
	if !channel.isclosed {
		channel.isclosed = true
		channel.connection.Close()
	}
}

// ActiveChannelPool keeps tracks of active UDP connections.
type ActiveChannelPool struct {
	mu         sync.RWMutex
	channelMap map[string]*channelWrapper
	pool       sync.Pool
}

type bufObj struct{ data []byte }

// NewChannelPool creates a shared pool to store active connections safely.
func NewChannelPool(Context context.Context, Config *RedirectionConfig) *ActiveChannelPool {
	pool := ActiveChannelPool{
		channelMap: map[string]*channelWrapper{},
		pool: sync.Pool{
			New: func() interface{} {
				return &bufObj{make([]byte, Config.BufferSize)}
			},
		},
	}
	pool.spawnGCRoutine(Context, Config.Timeout)
	return &pool
}

// Receiver takes an empty buffer as input, returns from which offset to start, the end offset, the identifier of the sender, or an error.
type Receiver func([]byte) (int, int, string, error)

// Sender takes a sender ID and bytes to be sent as input parameters, returns the number of bytes writen or an error.
type Sender func([]byte, string) (int, error)

// TunnelInitialier takes a sender ID as an input parameter, returns the bridged connection or an error.
type TunnelInitialier func(string) (net.Conn, error)

func (channelmap *ActiveChannelPool) getChannel(id string) *channelWrapper {
	channelmap.mu.RLock()
	defer channelmap.mu.RUnlock()
	if channel, exist := channelmap.channelMap[id]; exist && channel != nil && !channel.isClosed() {
		return channel
	}
	return nil
}

type channelSender struct {
	FanInSender Sender
	ID          string
	Channel     *channelWrapper
}

// Write implements an io.Writer method for io.copy.
func (sender *channelSender) Write(data []byte) (int, error) {
	sender.Channel.refreshActiveTimestamp()
	return sender.FanInSender(data, sender.ID)
}

func (channelmap *ActiveChannelPool) spawnGCRoutine(ctx context.Context, timeout time.Duration) {
	go func() {
		ticker := time.NewTicker(timeout)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				channelmap.mu.Lock()
				for _, conn := range channelmap.channelMap {
					if conn != nil {
						conn.close()
					}
				}
				channelmap.mu.Unlock()
				return
			case <-ticker.C:
				channelmap.mu.Lock()
				pendingRemove := []string{}
				for id, conn := range channelmap.channelMap {
					if conn == nil || conn.isClosed() || conn.outdated(timeout) {
						if conn != nil {
							conn.close()
						}
						pendingRemove = append(pendingRemove, id)
					}
				}
				for _, id := range pendingRemove {
					delete(channelmap.channelMap, id)
				}
				channelmap.mu.Unlock()
			}
		}
	}()
}

func (channelmap *ActiveChannelPool) lookupOrCreateChannel(ctx context.Context, id string, initialier TunnelInitialier, sender Sender) (*channelWrapper, error) {
	channel := channelmap.getChannel(id)
	if channel != nil && !channel.isClosed() {
		return channel, nil
	}
	conn, err := initialier(id)
	if err != nil {
		return nil, err
	}
	channel = &channelWrapper{
		connection: conn,
		lastActive: time.Now(),
		isclosed:   false,
	}
	channelmap.mu.Lock()
	channelmap.channelMap[id] = channel
	channelmap.mu.Unlock()
	go func() {
		if _, err := io.Copy(&channelSender{FanInSender: sender, ID: id, Channel: channel}, channel.connection); err != nil {
			if err != nil {
				log.Printf("error: %v", err)
			}
		}
		channel.close()
	}()
	go func() {
		<-ctx.Done()
		channel.close()
	}()
	return channel, nil
}

// RedirectFanIn will keep calling `FanInReceiver` to get a packet including data and a sender ID.
// Bridge the connection initialized with `TunnelInitialier` and returns its response back through `FanInSender`.
func RedirectFanIn(Context context.Context, ChannelPool *ActiveChannelPool, FanInReceiver Receiver, FanInSender Sender, TunnelInitialier TunnelInitialier) error {
	for Context.Err() == nil {
		buf := ChannelPool.pool.Get().(*bufObj)
		offset, n, senderID, err := FanInReceiver(buf.data)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return err
			}
			continue
		}
		go func(data *bufObj, offset int, n int, senderID string) {
			channel, err := ChannelPool.lookupOrCreateChannel(Context, senderID, TunnelInitialier, FanInSender)
			if err != nil {
				log.Print(err)
				return
			}

			if _, err := channel.write(data.data[offset:n]); err != nil {
				log.Print(err)
				return
			}
			ChannelPool.pool.Put(data)
		}(buf, offset, n, senderID)
	}
	return Context.Err()
}
