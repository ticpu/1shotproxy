package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/gosimple/slug"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	CacheFileSuffix  = ".cache"
	ProgramName      = "1shotproxy"
	SocketBufferSize = 8192
	SocketDeadline   = 30 * time.Second
)

type ProxyInstance struct {
	ListenAddress       string
	ConnectPort         string
	CacheLocation       string
	CacheTimeoutSeconds int64
	ProxyPool           sync.Pool
	CacheStaleTime      *time.Duration
	CacheFreshTime      *time.Duration
}

type ProxiedConnection struct {
	proxyInstance *ProxyInstance
	inbound       net.Conn
	inboundOpen   bool
	outbound      net.Conn
	outboundOpen  bool
	outboundIP    string
	socketName    string
	lastErr       error
	cacheFile     *os.File
	writeToCache  bool
	cacheUpToDate bool
	buffer        []byte
	destroy       func()
}

func isDirectory(path string) bool {
	if v, err := os.Stat(path); err == nil && v.IsDir() {
		return true
	}
	return false
}

func (conn *ProxiedConnection) failCacheFile() {
	conn.cacheUpToDate = false
	conn.writeToCache = false
	if conn.cacheFile != nil {
		log.Debugf("dropping cache file at %s", conn.cacheFile.Name())
		_ = conn.cacheFile.Close()
		_ = os.Remove(conn.cacheFile.Name())
	}
}

func isCacheRecentEnough(file *os.File, staleDuration time.Duration) bool {
	if fileInfo, err := file.Stat(); err != nil {
		log.Warningf("can't stat %s %s", file.Name(), err)
	} else {
		if time.Since(fileInfo.ModTime()) < staleDuration {
			return true
		}
	}
	return false
}

func (conn *ProxiedConnection) openCacheFile() {
	if conn.cacheFile != nil {
		return
	}

	var err error
	var filename = path.Join(conn.proxyInstance.CacheLocation, slug.Make(conn.outboundIP)) + CacheFileSuffix
	conn.cacheUpToDate = false
	conn.writeToCache = false

	if err = os.Mkdir(conn.proxyInstance.CacheLocation, 0755); err != nil && !os.IsExist(err) {
		log.Debugf("can't create cache directory at %s %s", conn.proxyInstance.CacheLocation, err)
	}

	if conn.cacheFile, err = os.OpenFile(filename, os.O_RDONLY, 0644); err != nil {
		log.Debugf("unable to open cache file at %s %s", filename, err)
	} else {
		if isCacheRecentEnough(conn.cacheFile, *conn.proxyInstance.CacheFreshTime) {
			log.Debugf("file is already up-to-date, obtaining lock")

			if err = unix.Flock(int(conn.cacheFile.Fd()), unix.LOCK_SH); err != nil {
				log.Warningf("couldn't obtain lock: %s", err)
			} else {
				conn.cacheUpToDate = true
				return
			}
		}
	}

	if conn.cacheFile != nil {
		_ = conn.cacheFile.Close()
	}

	_ = os.Remove(filename)

	if conn.cacheFile, err = os.OpenFile(filename, os.O_RDWR|unix.O_CREAT|unix.O_TRUNC, 0644); err != nil {
		log.Warningf("can't open %s for writing: %s", filename, err)
		return
	}

	if err = unix.Flock(int(conn.cacheFile.Fd()), unix.LOCK_EX); err != nil {
		log.Warningf("couldn't obtain lock: %s", err)
		return
	}

	log.Debugf("creating cache file at %s", filename)
	conn.writeToCache = true
	return
}

func (conn *ProxiedConnection) cacheReceivedData(data []byte, bytesRead int) {
	if conn.writeToCache == false {
		return
	}

	bytesWritten, err := conn.cacheFile.Write(data)

	if bytesWritten != bytesRead || err != nil {
		log.Warningf("couldn't complete write to %s %d != %d bytes and err is %s",
			conn.cacheFile.Name(), bytesWritten, bytesRead, err)
		conn.failCacheFile()
	}
}

func (conn *ProxiedConnection) openOutboundSocket() (net.Conn, error) {
	var err error

	if conn.outbound, err = net.DialTimeout("tcp", conn.outboundIP, SocketDeadline); err != nil {
		return nil, fmt.Errorf("unable to connect to %s: %s", conn.outboundIP, err)
	} else {
		conn.outboundOpen = true
		conn.socketName = fmt.Sprintf("%s -> %s", conn.inbound.RemoteAddr(), conn.outbound.RemoteAddr())
		log.Infof("connection established for %s", conn.socketName)
		return conn.outbound, nil
	}
}

func (conn *ProxiedConnection) proxyAndCache() {
	var bytesRead, bytesWritten int
	var err error
	var reader io.Reader

	conn.openCacheFile()

	if conn.cacheUpToDate == true {
		reader = conn.cacheFile
	} else {
		reader, err = conn.openOutboundSocket()

		if err != nil {
			log.Warning(err)
			return
		}
	}

	for {
		if bytesRead, err = reader.Read(conn.buffer); err != nil {
			if bytesRead == 0 {
				log.Debugf("read complete for %s", conn.socketName)
				conn.closeCacheFile()
			} else {
				log.Infof("read error for proxy %s: %s", conn.socketName, err)
			}
			break
		}

		conn.cacheReceivedData(conn.buffer[0:bytesRead], bytesRead)

		if bytesWritten, err = conn.inbound.Write(conn.buffer[0:bytesRead]); err != nil {
			log.Infof("write error for proxy %s: %s", conn.socketName, err)
			break
		} else {
			if bytesWritten != bytesRead {
				log.Warningf("incomplete write for proxy %s: %d != %d bytes",
					conn.socketName, bytesRead, bytesWritten)
				break
			}
		}
	}
}

func GetFdFromConn(conn net.Conn) (int, error) {
	var tcpSocket *net.TCPConn
	var socketFd *os.File
	var err error
	var ok bool

	if tcpSocket, ok = conn.(*net.TCPConn); !ok {
		return -1, fmt.Errorf("socket is not TCP")
	}
	if socketFd, err = tcpSocket.File(); err != nil {
		return -1, err
	}

	return int(socketFd.Fd()), nil
}

func (conn *ProxiedConnection) acceptProxiedConnection() {
	defer conn.destroy()
	var err error
	var socketFd int
	var outboundIP *net.Addr

	if socketFd, err = GetFdFromConn(conn.inbound); err != nil {
		log.Warningf("unable to get file descriptor from socket: %s", err)
		return
	}
	if outboundIP, err = GetSockOptInet4(socketFd, unix.IPPROTO_IP, SO_ORIGINAL_DST); err != nil {
		if outboundIP, err = GetSockOptInet6(socketFd, unix.IPPROTO_IPV6, SO_ORIGINAL_DST); err != nil {
			log.Warningf("couldn't find proxied connection: %s", err)
			return
		}
	}

	log.Infof("outbound to %s", *outboundIP)
	conn.outboundIP = strings.Replace((*outboundIP).String(), ":26557", ":26556", 1)
	conn.proxyAndCache()
}

func (conn *ProxiedConnection) closeCacheFile() {
	if conn.cacheFile != nil {
		if err := conn.cacheFile.Close(); err != nil {
			log.Warningf("couldn't close file %s %s, deleting the file", conn.cacheFile.Name(), err)
			if err := os.Remove(conn.cacheFile.Name()); err != nil {
				log.Warningf("couldn't delete file %s %s", conn.cacheFile.Name(), err)
			}
		}
		conn.cacheFile = nil
	}
}

func (pi *ProxyInstance) parseArguments() {
	pi.CacheLocation = os.Getenv("RUNTIME_DIRECTORY")

	if !isDirectory(pi.CacheLocation) {
		pi.CacheLocation = fmt.Sprintf("/tmp/%s", ProgramName)
	}

	logLevel := flag.String("log-level", "info",
		"log level between trace, debug, info, warn, error, fatal, panic")
	flag.StringVar(&(pi.ListenAddress), "listen", ":26557",
		"listen address and port")
	flag.StringVar(&(pi.ConnectPort), "connect-port", ":26556",
		"port to use for outgoing connection")
	flag.StringVar(&(pi.CacheLocation), "cache-dir", pi.CacheLocation,
		"directory to store cached data (RUNTIME_DIRECTORY environment variable)")
	pi.CacheFreshTime = flag.Duration("cache-fresh-time", 15*time.Second,
		"when to expire cache in seconds, 0 to disable")
	pi.CacheStaleTime = flag.Duration("cache-stale-time", 5*time.Minute,
		"how often to clean cache files")
	flag.Parse()

	logLevelValue, err := log.ParseLevel(*logLevel)
	if err != nil {
		log.Fatalf("invalid log level %s", *logLevel)
	}
	log.SetLevel(logLevelValue)

	if pi.CacheTimeoutSeconds > 0 && !isDirectory(pi.CacheLocation) {
		log.Fatalf("%s is not a valid directory", pi.CacheLocation)
	}
}

func (pi *ProxyInstance) newProxyConnection(inbound net.Conn) *ProxiedConnection {
	var conn = pi.ProxyPool.Get().(*ProxiedConnection)
	conn.proxyInstance = pi
	conn.inbound = inbound
	conn.outbound = nil
	conn.inboundOpen = true
	conn.outboundOpen = false
	conn.cacheFile = nil
	conn.writeToCache = true
	conn.cacheUpToDate = false
	for i := 0; i < SocketBufferSize; i++ {
		conn.buffer[i] = 0
	}
	conn.destroy = func() { pi.freeProxyConnection(conn) }
	return conn
}

func (pi *ProxyInstance) freeProxyConnection(conn *ProxiedConnection) {
	if conn.inbound != nil && conn.inboundOpen {
		if err := conn.inbound.Close(); err != nil {
			log.Infof("error while closing inbound connection: %s", err)
		} else {
			log.Debugf("inbound connection closed for %s", conn.socketName)
		}
	}

	if conn.outbound != nil && conn.outboundOpen {
		if err := conn.outbound.Close(); err != nil {
			log.Infof("error while closing outbound connection: %s", err)
		} else {
			log.Debugf("outbound connection closed for %s", conn.socketName)
		}
	}

	conn.failCacheFile()
	pi.ProxyPool.Put(conn)
}

func (pi *ProxyInstance) openListeningSocket() net.Listener {
	listenerConfig := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			err := c.Control(func(fd uintptr) {
				_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			})
			if err != nil {
				return err
			}
			return nil
		},
	}

	log.Infof("opening network socket at %s", pi.ListenAddress)
	listenSocket, err := listenerConfig.Listen(context.Background(), "tcp", pi.ListenAddress)

	if err != nil {
		log.Fatalf("error opening socket: %s", err)
	}

	return listenSocket
}

func (pi *ProxyInstance) dispatchConnections(listenSocket net.Listener) {
	var err error
	var newConnection net.Conn

	log.Debugf("waiting for connection...")

	for {
		newConnection, err = listenSocket.Accept()

		if err == nil {
			log.Infof("accepted connection from %s", newConnection.RemoteAddr())
			conn := pi.newProxyConnection(newConnection)
			go conn.acceptProxiedConnection()
		} else {
			log.Warningf("couldn't not accept connection: %s", err)
		}
	}
}

func cleanStaleFiles(cacheDir string, cleanTime time.Duration) {
	var err error
	var toDelete *os.File
	var fileList []os.DirEntry
	var fileInfo os.FileInfo
	var toDeletePath string

	for {
		log.Debugf("cleaning stale cache files")
		fileList, err = os.ReadDir(cacheDir)

		if err != nil {
			log.Errorf("can't open cache state directory %s for cleanup: %s", cacheDir, err)
			break
		}

		for _, file := range fileList {
			if !file.IsDir() && strings.HasSuffix(file.Name(), CacheFileSuffix) {
				toDeletePath = path.Join(cacheDir, file.Name())
				toDelete, err = os.Open(toDeletePath)

				if err == nil {
					fileInfo, err = toDelete.Stat()
					if err == nil {
						if time.Since(fileInfo.ModTime()) > cleanTime {
							log.Debugf("deleting %s", toDeletePath)
							_ = toDelete.Close()
							_ = os.Remove(toDeletePath)
						}
					}
				}
			}
		}
		log.Debugf("waiting for %0.0f seconds", cleanTime.Seconds())
		time.Sleep(cleanTime)
	}
}

func main() {
	var pi ProxyInstance
	pi.parseArguments()
	pi.ProxyPool = sync.Pool{
		New: func() interface{} {
			conn := new(ProxiedConnection)
			conn.buffer = make([]byte, SocketBufferSize)
			return conn
		},
	}

	go cleanStaleFiles(pi.CacheLocation, *pi.CacheStaleTime)
	listenSocket := pi.openListeningSocket()
	pi.dispatchConnections(listenSocket)
}
