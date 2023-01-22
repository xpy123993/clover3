package fan_test

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/xpy123993/clover3/fan"
)

func TestBasic(t *testing.T) {
	counter := 0
	wg := sync.WaitGroup{}
	wg.Add(20)
	err := fan.RedirectFanIn(context.Background(), fan.NewChannelPool(context.Background(), &fan.RedirectionConfig{Timeout: time.Millisecond, BufferSize: 4096}), func(b []byte) (int, int, string, error) {
		b[0] = byte(counter)
		counter++
		if counter > 10 {
			return 0, -1, "", io.EOF
		}
		return 0, 1, strconv.Itoa(counter - 1), nil
	}, func(b []byte, s string) (int, error) {
		idx, err := strconv.Atoi(s)
		if err != nil {
			t.Error(err)
			return -1, err
		}
		if len(b) != 1 || b[0] != byte(idx) {
			t.Errorf("%d", idx)
		}
		wg.Done()
		return 1, nil
	}, func(string) (net.Conn, error) {
		peerA, peerB := net.Pipe()
		go func() {
			p := make([]byte, 1)
			_, err := peerA.Read(p)
			if err != nil {
				t.Error(err)
			}
			peerA.Write(p)
			time.Sleep(10 * time.Millisecond)
			if _, err := peerA.Write(p); err != io.ErrClosedPipe {
				t.Error(err)
			}
			wg.Done()
		}()
		return peerB, nil
	})
	if err != io.EOF {
		t.Errorf("unexpect error: %v", err)
	}
	wg.Wait()
}
