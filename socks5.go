package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"time"

	"github.com/xpy123993/clover3/fan"
)

func writeIPAndPort(Conn io.Writer, Addr net.Addr) error {
	var ip net.IP
	var port int
	if addr, ok := Addr.(*net.TCPAddr); ok {
		ip = addr.IP
		port = addr.Port
	} else if addr, ok := Addr.(*net.UDPAddr); ok {
		ip = addr.IP
		port = addr.Port
	} else {
		return fmt.Errorf("unknown addr: %v", Addr)
	}
	if ipb := ip.To4(); ipb != nil {
		if _, err := Conn.Write([]byte{1}); err != nil {
			return err
		}
		if _, err := Conn.Write(ipb); err != nil {
			return err
		}
	} else if ipb := ip.To16(); ipb != nil {
		if _, err := Conn.Write([]byte{4}); err != nil {
			return err
		}
		if _, err := Conn.Write(ipb); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("unknown ip: %v", ip)
	}
	if _, err := Conn.Write([]byte{byte(port >> 8), byte(port & 0xff)}); err != nil {
		return err
	}
	return nil
}

func readProxyAddress(conn io.Reader) (string, error) {
	singleByte := make([]byte, 1)
	target := ""
	if _, err := conn.Read(singleByte); err != nil {
		return "", err
	}
	switch singleByte[0] {
	case 1:
		ipbuf := make([]byte, 4)
		if _, err := io.ReadFull(conn, ipbuf); err != nil {
			return "", err
		}
		target = net.IP(ipbuf).String()
	case 3:
		if _, err := conn.Read(singleByte); err != nil {
			return "", err
		}
		hostname := make([]byte, singleByte[0])
		if _, err := io.ReadFull(conn, hostname); err != nil {
			return "", err
		}
		target = string(hostname)
	case 4:
		ipbuf := make([]byte, 16)
		if _, err := io.ReadFull(conn, ipbuf); err != nil {
			return "", err
		}
		target = net.IP(ipbuf).String()
	}
	portByte := make([]byte, 2)
	if _, err := io.ReadFull(conn, portByte); err != nil {
		return "", err
	}
	return target + ":" + strconv.Itoa(int(portByte[0])<<8+int(portByte[1])), nil
}

// ClientSession abtracts all the operations on a client connection.
type ClientSession struct {
	Conn net.Conn
}

// socks5auth initiates the first handshake with authentication (no auth).
func (session *ClientSession) socks5auth() error {
	singleByte := make([]byte, 1)
	if _, err := session.Conn.Read(singleByte); err != nil || singleByte[0] != 5 {
		return err
	}
	if _, err := session.Conn.Read(singleByte); err != nil {
		return err
	}
	if _, err := io.ReadFull(session.Conn, make([]byte, singleByte[0])); err != nil {
		return err
	}
	if _, err := session.Conn.Write([]byte{5, 0}); err != nil {
		return err
	}
	return nil
}

func (session *ClientSession) close() {
	session.Conn.Close()
}

func (session *ClientSession) readRequest() (byte, string, error) {
	head := make([]byte, 3)
	if _, err := io.ReadFull(session.Conn, head); err != nil || head[0] != 5 || (head[1] != 1 && head[1] != 3) || head[2] != 0 {
		if err != nil {
			return 0, "", err
		}
		return 0, "", fmt.Errorf("invalid request")
	}

	remoteAddress, err := readProxyAddress(session.Conn)
	if err != nil {
		return 0, "", err
	}
	return head[1], remoteAddress, nil
}

func (session *ClientSession) prepareRelayTCP(RuntimeContext context.Context, RemoteAddress string, Dialer func(network, address string) (net.Conn, error)) (net.Conn, error) {
	conn, err := Dialer("tcp", RemoteAddress)
	if err != nil {
		return nil, fmt.Errorf("cannot handshake with proxy server: %v", err)
	}
	writer := new(bytes.Buffer)
	if _, err := writer.Write([]byte{5, 0, 0}); err != nil {
		conn.Close()
		return nil, err
	}
	bindAddr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := writeIPAndPort(writer, bindAddr); err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := writer.WriteTo(session.Conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func (session *ClientSession) rejectRequest() {
	session.Conn.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0})
}

// StartProxyClient creates a local socks5 service, and forward traffic to proxy server on `Channel`.
func StartProxyClient(RuntimeContext context.Context, Dialer func(network, address string) (net.Conn, error), LocalAddress string) error {
	listener, err := net.Listen("tcp", LocalAddress)
	if err != nil {
		return err
	}
	go func() {
		<-RuntimeContext.Done()
		listener.Close()
	}()
	return StartProxyClientWithListener(RuntimeContext, Dialer, LocalAddress, listener)
}

// StartProxyClientWithListener starts a socks5 proxy on `listener`.
func StartProxyClientWithListener(RuntimeContext context.Context, Dialer func(network, address string) (net.Conn, error), LocalAddress string, listener net.Listener) error {
	udpAddr, err := net.ResolveUDPAddr("udp", LocalAddress)
	if err != nil {
		return err
	}

	for RuntimeContext.Err() == nil {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go func(conn net.Conn) {
			session := ClientSession{Conn: conn}
			defer session.close()

			if err := session.socks5auth(); err != nil {
				log.Printf("failed to handshake: %v", err)
				return
			}

			requestType, remoteAddress, err := session.readRequest()
			if err != nil {
				log.Printf("failed to read request: %v", err)
				return
			}

			switch requestType {
			case 1:
				remoteConn, err := session.prepareRelayTCP(RuntimeContext, remoteAddress, Dialer)
				if err != nil {
					session.rejectRequest()
					return
				}
				defer remoteConn.Close()
				ctx, cancelFn := context.WithCancel(RuntimeContext)
				go func() { io.Copy(session.Conn, remoteConn); cancelFn() }()
				go func() { io.Copy(remoteConn, session.Conn); cancelFn() }()
				<-ctx.Done()
			case 3:
				log.Printf("Redirecting UDP")
				udpConn, err := net.ListenUDP("udp", &net.UDPAddr{
					IP:   udpAddr.IP,
					Port: 0,
				})
				if err != nil {
					session.rejectRequest()
					return
				}
				writer := new(bytes.Buffer)
				if _, err := writer.Write([]byte{5, 0, 0}); err != nil {
					return
				}
				if err := writeIPAndPort(writer, udpConn.LocalAddr()); err != nil {
					return
				}
				writer.WriteTo(conn)
				serveContext, cancelFn := context.WithCancel(RuntimeContext)
				go func() {
					conn.Read(make([]byte, 1))
					udpConn.Close()
					cancelFn()
				}()
				var localAddr *net.UDPAddr

				channelPool := fan.NewChannelPool(serveContext, &fan.RedirectionConfig{Timeout: time.Second * 30, BufferSize: 65536})
				fan.RedirectFanIn(serveContext, channelPool, func(buf []byte) (int, int, string, error) {
					n, sender, err := udpConn.ReadFromUDP(buf)
					if err != nil {
						return -1, -1, "", err
					}
					reader := bytes.NewReader(buf)
					for i := 0; i < 3; i++ {
						if b, err := reader.ReadByte(); err != nil || b != 0 {
							if err != nil {
								return -1, -1, "", err
							}
							return -1, -1, "", fmt.Errorf("invalid request")
						}
					}
					localAddr = sender
					addr, err := readProxyAddress(reader)
					if err != nil {
						return -1, -1, "", err
					}
					return (len(buf) - reader.Len()), n, addr, nil
				}, func(b []byte, s string) (int, error) {
					writer := new(bytes.Buffer)
					writer.Write([]byte{0, 0, 0})
					udpAddr, err := net.ResolveUDPAddr("udp", s)
					if err != nil {
						return -1, err
					}
					if err := writeIPAndPort(writer, udpAddr); err != nil {
						return -1, err
					}
					writer.Write(b)
					total := writer.Len()
					n, err := udpConn.WriteToUDP(writer.Bytes(), localAddr)
					return len(b) - (total - n), err
				}, func(s string) (net.Conn, error) {
					peerConn, err := Dialer("udp", s)
					go func() {
						<-serveContext.Done()
						peerConn.Close()
					}()
					return peerConn, err
				})
			default:
				session.rejectRequest()
			}
		}(conn)
	}
	return nil
}
