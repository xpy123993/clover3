package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"git.yuki.nu/corenet"
	"github.com/google/uuid"
	"github.com/lucas-clemente/quic-go"
	"golang.org/x/net/trace"
)

var (
	cmdFlags        = flag.NewFlagSet("clover3", flag.ExitOnError)
	serverRelay     = cmdFlags.Bool("serve-bridge", false, "If true, a relay server will be created to serve `bridge-url`.")
	relayServerURLs = cmdFlags.String("bridge-url", "", "The URL of the relay server. Can be multiple splitted by `,`")

	channel             = cmdFlags.String("endpoint-channel", "", "If specified, an endpoint service will be created on that channel.")
	serverLocalPort     = cmdFlags.Int("endpoint-channel-direct-port", -1, "If non-negative and channel is not empty, the endpoint server will also listen on a direct port.")
	localSocks5AddrPair = cmdFlags.String("socks5-list", "", "The list of [channel]:[local socks5 ports], splitted by `,`")
	exposeLocalAddr     = cmdFlags.Bool("socks5-public", false, "If true, socks5 port will be served at 0.0.0.0. By default only listen on 127.0.0.1")
	debugPprof          = cmdFlags.String("pprof-address", "", "If not empty, a web server will be started to provide pprof.")
	socks5DialTimeout   = cmdFlags.Duration("socks5-dial-timeout", 10*time.Second, "The timeout for the proxy server to dial to an address.")
	ramdomizeChannel    = cmdFlags.Bool("endpoint-randomize-channel", false, "If true, an UUID will be added as a suffix of the channel.")
	logError            = cmdFlags.Bool("log-error", false, "If true, the server will dump errors")
	templateTLSConfig   *tls.Config

	exitSig     = make(chan struct{}, 1)
	relayServer *corenet.RelayServer
)

func serveRelay() error {
	relayServer = corenet.NewRelayServer(
		corenet.WithRelayServerForceEvictChannelSession(true),
		corenet.WithRelayServerLogError(*logError))

	serverURLs := strings.Split(*relayServerURLs, ",")
	for _, rawURL := range serverURLs {
		serverURL, err := url.Parse(rawURL)
		if err != nil {
			return err
		}
		go func() {
			port := serverURL.Port()
			if len(port) == 0 {
				port = "13300"
			}
			serviceAddress := fmt.Sprintf(":%s", port)
			switch serverURL.Scheme {
			case "ttf":
				relayServerListener, err := tls.Listen("tcp", serviceAddress, templateTLSConfig)
				if err != nil {
					log.Printf("Failed to bind %s: %v", serverURL.String(), err)
					break
				}
				defer relayServerListener.Close()
				log.Printf("Relay service is serving on `%s://%s`", serverURL.Scheme, relayServerListener.Addr().String())
				log.Printf("Relay service returns status: %v", relayServer.Serve(relayServerListener, corenet.UsePlainRelayProtocol()))
			case "ktf":
				relayServerListener, err := corenet.CreateRelayKCPListener(serviceAddress, templateTLSConfig, corenet.DefaultKCPConfig())
				if err != nil {
					log.Printf("Failed to bind %s: %v", serverURL.String(), err)
					break
				}
				defer relayServerListener.Close()
				log.Printf("Relay service is serving on `%s://%s`", serverURL.Scheme, relayServerListener.Addr().String())
				log.Printf("Relay service returns status: %v", relayServer.Serve(relayServerListener, corenet.UseKCPRelayProtocol()))
			case "quicf":
				relayServerListener, err := corenet.CreateRelayQuicListener(serviceAddress, templateTLSConfig, &quic.Config{KeepAlivePeriod: 20 * time.Second})
				if err != nil {
					log.Printf("Failed to bind %s: %v", serverURL.String(), err)
					break
				}
				defer relayServerListener.Close()
				log.Printf("Relay service is serving on `%s://%s`", serverURL.Scheme, relayServerListener.Addr().String())
				log.Printf("Relay service returns status: %v", relayServer.Serve(relayServerListener, corenet.UseQuicRelayProtocol()))
			}
			log.Printf("Relay service on `%s` is stopped", serverURL.String())
			exitSig <- struct{}{}
		}()
	}
	return nil
}

func serveEndpointService(channelName string) error {
	adapters := []corenet.ListenerAdapter{}
	if *serverLocalPort >= 0 {
		directAdapter, err := corenet.CreateListenerTCPPortAdapter(*serverLocalPort)
		if err != nil {
			log.Printf("Warning: listening on local port failed: %v", err)
		} else {
			adapters = append(adapters, directAdapter)
		}
	}
	serverURLs := strings.Split(*relayServerURLs, ",")
	listenerFallbackOptions := &corenet.ListenerFallbackOptions{
		TLSConfig: templateTLSConfig,
		KCPConfig: corenet.DefaultKCPConfig(),
		QuicConfig: &quic.Config{
			KeepAlivePeriod: 20 * time.Second,
		},
	}
	for _, serverURL := range serverURLs {
		relayAdapter, err := corenet.CreateListenerFallbackURLAdapter(serverURL, channelName, listenerFallbackOptions)
		if err != nil {
			log.Printf("Warning: listening on %s failed: %v", serverURL, err)
		}
		adapters = append(adapters, relayAdapter)

		if *serverRelay && len(*relayServerURLs) > 1 {
			log.Printf("The program is also configured to run a relay server, skipped other connections to the local server.")
			break
		}
	}
	if len(adapters) == 0 {
		return fmt.Errorf("no active listeners")
	}
	listener := corenet.NewMultiListener(adapters...)
	defer listener.Close()
	return handleProxyServer(tls.NewListener(listener, templateTLSConfig))
}

type trackConn struct {
	net.Conn

	mu       sync.Mutex
	isClosed bool
	tracker  trace.Trace
}

func (c *trackConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed {
		return nil
	}
	c.isClosed = true
	c.tracker.Finish()
	return c.Conn.Close()
}

func serveLocalSocks5(channel, localAddr string, dialer *corenet.Dialer, tlsConfig *tls.Config) error {
	log.Printf("Socks5 service `%s` -> `%s`", channel, localAddr)
	return StartProxyClient(context.Background(), func(network, address string) (net.Conn, error) {
		tracker := trace.New(channel, fmt.Sprintf("Socks5 connection to %s", address))
		conn, err := proxyDial(dialer, channel, network, address, tlsConfig, tracker)
		if err != nil {
			tracker.Finish()
			return nil, err
		}
		return &trackConn{Conn: conn, tracker: tracker}, nil
	}, localAddr)
}

func initialize() error {
	tlsConfig, err := getTLSConfigFromEnv()
	if err == nil {
		templateTLSConfig = tlsConfig
		return nil
	}
	tlsConfig, err = getTLSConfigFromEmbeded()
	if err == nil {
		templateTLSConfig = tlsConfig
		return nil
	}
	return fmt.Errorf("no available token, last error: %v", err)
}

func main() {
	defer close(exitSig)
	trace.AuthRequest = func(req *http.Request) (any bool, sensitive bool) {
		return true, true
	}

	if data, err := embeddedFile.ReadFile("tokens/cmdline.txt"); err == nil {
		cmdFlags.Parse(strings.Split(string(data), "\n"))
		if len(os.Args) > 1 {
			log.Printf("WARNING: This binary is compiled with built-in configs, all command arguments will be ignored")
		}
	} else {
		cmdFlags.Parse(os.Args[1:])
	}

	if err := initialize(); err != nil {
		log.Print(err)
		return
	}

	certName, err := getServerName(templateTLSConfig.Certificates[0].Certificate[0])
	if err != nil {
		log.Printf("Failed to read the certificate: %v", err)
		return
	}
	log.Printf("SSO: I am %s", certName)

	if len(*debugPprof) > 0 {
		go func() {
			lis, err := net.Listen("tcp", *debugPprof)
			if err != nil {
				log.Printf("Failed to start debug server: %v", err)
			}
			if err := http.Serve(lis, nil); err != nil {
				log.Printf("Webserver returns error: %v", err)
			}
		}()
	}

	if len(*relayServerURLs) == 0 {
		cmdFlags.Usage()
		return
	}

	osSignals := make(chan os.Signal, 1)
	defer close(osSignals)
	signal.Notify(osSignals, syscall.SIGABRT, syscall.SIGINT, syscall.SIGTERM)

	taskCounter := 0
	if *serverRelay {
		taskCounter++
		if err := serveRelay(); err != nil {
			log.Print(err)
			return
		}
	}

	if len(*channel) > 0 {
		if *channel != certName && !strings.HasPrefix(*channel, certName+"@") {
			log.Printf("WARNING: channel name mismatch: Client might not trust the service")
		}
		taskCounter++
		channelName := *channel
		if *ramdomizeChannel {
			channelName = channelName + "@" + uuid.New().String()
		}
		go func() {
			if err := serveEndpointService(channelName); err != nil {
				log.Printf("Endpoint service exited with error: %v", err)
			}
			exitSig <- struct{}{}
		}()
	}

	if len(*localSocks5AddrPair) > 0 {
		dialer := corenet.NewDialer(strings.Split(*relayServerURLs, ","),
			corenet.WithDialerRelayTLSConfig(templateTLSConfig))
		defer dialer.Close()
		addressTuple := strings.Split(*localSocks5AddrPair, ",")
		for _, address := range addressTuple {
			channel, port, err := net.SplitHostPort(address)
			if err != nil {
				log.Printf("Cannot parse address tuples: %v", err)
				return
			}
			localAddr := fmt.Sprintf("127.0.0.1:%s", port)
			if *exposeLocalAddr {
				localAddr = fmt.Sprintf(":%s", port)
			}
			taskCounter++
			go func() {
				channelTLSConfig := templateTLSConfig.Clone()
				channelTLSConfig.ServerName = channel
				if strings.Contains(channel, "@") {
					channelTLSConfig.ServerName = channel[:strings.Index(channel, "@")]
				}
				if err := serveLocalSocks5(channel, localAddr, dialer, channelTLSConfig); err != nil {
					log.Printf("socks5 service (%s) exited with error: %v", channel, err)
				}
				exitSig <- struct{}{}
			}()
		}
	}
	if taskCounter == 0 {
		log.Printf("No pending work, exited.")
		return
	}
	select {
	case <-exitSig:
		log.Printf("One of the services exited.")
	case res := <-osSignals:
		log.Printf("Received signal: %v", res)
	}
	if relayServer != nil {
		relayServer.Close()
	}
	log.Printf("Exited.")
}
