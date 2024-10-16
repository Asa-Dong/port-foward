package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

var (
	confForward string
	showHelp    bool
)

func init() {
	flag.StringVar(&confForward, "f", "", "forward ports eg. udp::53-8.8.8.8:53,tcp:0.0.0.0:80-x.x.x.x:80")
	flag.BoolVar(&showHelp, "help", false, "show help")
	flag.Parse()

	log.SetFlags(log.Lshortfile | log.LstdFlags)
}

func parse(g string) (*Forward, error) {
	tmp := strings.Split(strings.TrimSpace(g), "-")
	if len(tmp) != 2 {
		return nil, fmt.Errorf("invalid format")
	}

	// remote
	remote := tmp[1]
	if len(strings.Split(remote, ":")) != 2 {
		return nil, fmt.Errorf("invalid format")
	}

	// protocol, local
	left := strings.Split(tmp[0], ":")
	if len(left) != 3 {
		return nil, fmt.Errorf("invalid format")
	}
	protocol := left[0]
	local := left[1]
	port := left[2]
	return &Forward{
		LocalAddr:   fmt.Sprintf("%s:%s", local, port),
		RemoteAddr:  remote,
		Protocol:    protocol,
		udpRConnMap: make(map[string]*net.UDPConn),
	}, nil
}

func main() {
	if showHelp || confForward == "" {
		flag.Usage()
		return
	}

	var forwards []*Forward
	gs := strings.Split(confForward, ",")
	for _, g := range gs {
		f, err := parse(g)
		if err != nil {
			log.Fatalf("'%s' %s", g, err)
		}
		forwards = append(forwards, f)
	}
	if len(forwards) == 0 {
		log.Fatalf("forward is empty")
	}

	g := sync.WaitGroup{}
	for _, f := range forwards {
		g.Add(1)
		go func(f *Forward) {
			defer g.Done()
			err := f.Serve()
			if err != nil {
				log.Printf("[%s] serve failed. err: %v\n", f, err)
			}
		}(f)
	}
	g.Wait()
}

type Forward struct {
	LocalAddr  string
	RemoteAddr string
	Protocol   string

	lock        sync.Mutex
	udpRConnMap map[string]*net.UDPConn // map[clientAddr]remoteConn
}

func (f *Forward) String() string {
	return fmt.Sprintf("%s:%s-%s", f.Protocol, f.LocalAddr, f.RemoteAddr)
}

func (f *Forward) Serve() error {
	if strings.HasPrefix(f.Protocol, "tcp") {
		return f.serveTcp()
	}
	return f.serveUdp()
}

func (f *Forward) serveTcp() error {
	ln, err := net.Listen(f.Protocol, f.LocalAddr)
	if err != nil {
		return err
	}
	defer ln.Close()

	// test remote ?
	rn, err := net.Dial(f.Protocol, f.RemoteAddr)
	if err != nil {
		return err
	}
	rn.Close()

	log.Printf("[%s] listen on %s\n", f, ln.Addr().String())
	var retryDelay time.Duration
	maxDelay := 1 * time.Second
	var id int
	var lc net.Conn
	for {
		lc, err = ln.Accept()
		if err != nil {
			if retryDelay == 0 {
				retryDelay = 50 * time.Millisecond
			} else {
				retryDelay *= 2
			}
			if retryDelay > maxDelay {
				retryDelay = maxDelay
			}
			log.Printf("[%s] accept error: %v; retrying in %v\n", f, err, retryDelay)
			time.Sleep(retryDelay)
			continue
		}

		id++
		log.Printf("[%s] [%d] [%s -> -] new conn\n", f, id, lc.RemoteAddr())
		go f.forwardTcp(id, lc)
	}
}

func (f *Forward) serveUdp() error {
	localAddr, err := net.ResolveUDPAddr(f.Protocol, f.LocalAddr)
	if err != nil {
		return err
	}

	remoteAddr, err := net.ResolveUDPAddr(f.Protocol, f.RemoteAddr)
	if err != nil {
		return err
	}

	lc, err := net.ListenUDP(f.Protocol, localAddr)
	if err != nil {
		return err
	}
	defer lc.Close()

	log.Printf("[%s] listen on %s\n", f, localAddr)

	buf := make([]byte, 32*1024)
	for {
		n, clientAddr, err := lc.ReadFromUDP(buf)
		if err != nil {
			log.Printf("[%s] read err: %s\n", f, err)
			continue
		}
		go f.forwardUdp(clientAddr, remoteAddr, lc, buf[:n])
	}

}

func (f *Forward) forwardUdp(clientAddr *net.UDPAddr, remoteAddr *net.UDPAddr, lc *net.UDPConn, buf []byte) {
	var rc *net.UDPConn
	var err error
	var isNewConn bool
	const timeout = time.Second * 30

	clientAddrStr := clientAddr.String()

	f.lock.Lock()
	if cacheRC, ok := f.udpRConnMap[clientAddrStr]; ok {
		rc = cacheRC
	} else {
		rc, err = net.DialUDP(f.Protocol, nil, remoteAddr)
		if err != nil {
			log.Printf("[%s] dial remote failed. err:%v", f, err)
			f.lock.Unlock()
			return
		}
		f.udpRConnMap[clientAddr.String()] = rc
		isNewConn = true
		log.Printf("[%s] [%s <> %s] new remote conn", f, clientAddr, remoteAddr)
	}
	// delay deadLine
	rc.SetDeadline(time.Now().Add(timeout))
	f.lock.Unlock()

	n, err := rc.Write(buf)
	if err != nil {
		log.Printf("[%s] Write remote failed. err:%v", f, err)
		return
	}
	log.Printf("[%s] [%s -> %s] forward %d bytes", f, clientAddr, remoteAddr, n)

	if !isNewConn {
		return
	}

	go func() {
		defer func() {
			f.lock.Lock()
			delete(f.udpRConnMap, clientAddrStr)
			f.lock.Unlock()

			rc.Close()
			log.Printf("[%s] [%s <> %s] remote closed ", f, clientAddr, remoteAddr)
			return
		}()

		buf = make([]byte, 32*1024)
		for {
			n, err := rc.Read(buf)
			if err != nil {
				if e, ok := err.(net.Error); ok && e.Timeout() {
					return
				}
				log.Printf("[%s] [ <- %s] read remote err: %v", f, remoteAddr, err)
				return
			}
			_, err = lc.WriteToUDP(buf[:n], clientAddr)
			if err != nil {
				log.Printf("[%s] [%s <- %s] write client err: %v", f, clientAddr, remoteAddr, err)
				return
			}
			log.Printf("[%s] [%s <- %s] forward %d bytes", f, clientAddr, remoteAddr, n)
		}
	}()

}

func (f *Forward) forwardTcp(id int, lc net.Conn) {
	var rc net.Conn

	defer func() {
		lc.Close()
		if rc != nil {
			rc.Close()
		}
		log.Printf("[%s] [%v] [%s <> %s] closed", f, id, lc.RemoteAddr().String(), rc.RemoteAddr().String())
	}()

	var err error
	var retryDelay time.Duration // how long to sleep on accept failure
	maxRetryTimes, retryTimes := 3, 0
	for {
		rc, err = net.Dial(f.Protocol, f.RemoteAddr)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() && retryTimes < maxRetryTimes {
				if retryDelay == 0 {
					retryDelay = 100 * time.Millisecond
				} else {
					retryDelay *= 2
				}
				log.Printf("[%s] [%d] [%s -> %s] conn remote failed. err:%v; retryTimes:%v, retrying in %v\n",
					f, id, lc.RemoteAddr(), rc.RemoteAddr(), err, retryTimes, retryDelay)
				time.Sleep(retryDelay)
				retryTimes++
				continue
			}
			log.Printf("[%s] [%d] [%s -> %s] conn remote failed. err:%v\n", f, id, lc.RemoteAddr(), rc.RemoteAddr(), err)
			return
		}
		break
	}

	log.Printf("[%s] [%d] [%s -> %s] conn success", f, id, lc.RemoteAddr(), rc.RemoteAddr())

	go io.Copy(lc, rc)
	io.Copy(rc, lc)
}
