package main

import (
	"context"
	"crypto/tls"
	"encoding/gob"
	"fmt"
	"io"
	"log"
	"net"

	"github.com/xpy123993/corenet"
	"golang.org/x/net/trace"
)

type request struct {
	Method  string
	Address string
}

type response struct {
	Success bool
	Payload string
}

func proxyDial(dialer *corenet.Dialer, channel, network, remoteAddress string, tlsConfig *tls.Config, tracker trace.Trace) (net.Conn, error) {
	conn, err := dialer.Dial(channel)
	if err != nil {
		tracker.LazyPrintf("Cannot reach out to remote server: %v", err)
		tracker.SetError()
		return nil, err
	}
	conn = tls.Client(conn, tlsConfig)

	handshakeSuccess := false
	defer func() {
		if !handshakeSuccess {
			tracker.SetError()
			conn.Close()
		}
	}()

	if err := gob.NewEncoder(conn).Encode(request{Method: network, Address: remoteAddress}); err != nil {
		tracker.LazyPrintf("Cannot send request: %v", err)
		return nil, err
	}
	tracker.LazyPrintf("Request sent.")
	resp := response{}
	if err := gob.NewDecoder(conn).Decode(&resp); err != nil {
		tracker.LazyPrintf("Cannot decode response: %v", err)
		return nil, err
	}
	if !resp.Success {
		tracker.LazyPrintf("Remote server returns error: %v", resp.Payload)
		return nil, fmt.Errorf("remote error: %s", resp.Payload)
	}
	tracker.LazyPrintf("Remote server returns OK. Remote address: %s", resp.Payload)
	handshakeSuccess = true
	return conn, nil
}

func handleProxyServer(listener net.Listener) error {
	log.Printf("Serving on address %s", listener.Addr().String())
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go func(clientconn net.Conn) {
			defer clientconn.Close()
			req := request{}
			if gob.NewDecoder(clientconn).Decode(&req) != nil {
				gob.NewEncoder(clientconn).Encode(response{Success: false, Payload: "invaild format"})
				return
			}
			remoteConn, err := net.DialTimeout(req.Method, req.Address, *socks5DialTimeout)
			if err != nil {
				gob.NewEncoder(clientconn).Encode(response{Success: false, Payload: err.Error()})
				return
			}
			defer remoteConn.Close()
			if gob.NewEncoder(clientconn).Encode(response{Success: true, Payload: remoteConn.LocalAddr().String()}) != nil {
				return
			}
			ctx, cancelFn := context.WithCancel(context.Background())
			go func() { io.Copy(clientconn, remoteConn); cancelFn() }()
			go func() { io.Copy(remoteConn, clientconn); cancelFn() }()
			<-ctx.Done()
		}(conn)
	}
}
