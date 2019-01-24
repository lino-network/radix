package radix

import (
	"fmt"
	"net"
	"sync"
	"time"
)

type sentinelOpts struct {
	cf ConnFunc
	pf ClientFunc
}

// SentinelOpt is an optional behavior which can be applied to the NewSentinel
// function to effect a Sentinel's behavior.
type SentinelOpt func(*sentinelOpts)

// SentinelConnFunc tells the Sentinel to use the given ConnFunc when connecting
// to sentinel instances.
func SentinelConnFunc(cf ConnFunc) SentinelOpt {
	return func(so *sentinelOpts) {
		so.cf = cf
	}
}

// SentinelPoolFunc tells the Sentinel to use the given ClientFunc when creating
// a pool of connections to the sentinel's primary.
func SentinelPoolFunc(pf ClientFunc) SentinelOpt {
	return func(so *sentinelOpts) {
		so.pf = pf
	}
}

// Sentinel is a Client which, in the background, connects to an available
// sentinel node and handles all of the following:
//
// * Creates a pool to the current primary instance, as advertised by the
// sentinel
//
// * Listens for events indicating the primary has changed, and automatically
// creates a new Client to the new primary
//
// * Keeps track of other sentinels in the cluster, and uses them if the
// currently connected one becomes unreachable
//
type Sentinel struct {
	so        sentinelOpts
	initAddrs []string
	name      string

	// we read lock when calling methods on prim, and normal lock when swapping
	// the value of prim, primAddr, and sentAddrs
	l             sync.RWMutex
	prim          Client
	primAddr      string
	secsM         map[string]bool
	sentinelAddrs map[string]bool // the known sentinel addresses

	// We use a persistent PubSubConn here, so we don't need to do much after
	// initialization. The pconn is only really kept around for closing
	pconn   PubSubConn
	pconnCh chan PubSubMessage

	// Any errors encountered internally will be written to this channel. If
	// nothing is reading the channel the errors will be dropped. The channel
	// will be closed when the Close is called.
	ErrCh chan error

	closeCh   chan bool
	closeWG   sync.WaitGroup
	closeOnce sync.Once

	// only used by tests to ensure certain actions have happened before
	// continuing on during the test
	testEventCh chan string
}

// NewSentinel creates and returns a *Sentinel instance. NewSentinel takes in a
// number of options which can overwrite its default behavior. The default
// options NewSentinel uses are:
//
//	SentinelConnFunc(func(net, addr string) (Conn, error) {
//		return Dial(net, addr, DialTimeout(5 * time.Second))
//	})
//	SentinelPoolFunc(DefaultClientFunc)
//
func NewSentinel(primaryName string, sentinelAddrs []string, opts ...SentinelOpt) (*Sentinel, error) {
	addrs := map[string]bool{}
	for _, addr := range sentinelAddrs {
		addrs[addr] = true
	}

	sc := &Sentinel{
		initAddrs:     sentinelAddrs,
		name:          primaryName,
		sentinelAddrs: addrs,
		pconnCh:       make(chan PubSubMessage, 1),
		ErrCh:         make(chan error, 1),
		closeCh:       make(chan bool),
		testEventCh:   make(chan string, 1),
	}

	defaultSentinelOpts := []SentinelOpt{
		SentinelConnFunc(func(net, addr string) (Conn, error) {
			return Dial(net, addr, DialTimeout(5*time.Second))
		}),
		SentinelPoolFunc(DefaultClientFunc),
	}

	for _, opt := range append(defaultSentinelOpts, opts...) {
		// the other args to NewSentinel used to be a ConnFunc and a ClientFunc,
		// which someone might have left as nil, in which case this now gives a
		// weird panic. Just handle it
		if opt != nil {
			opt(&(sc.so))
		}
	}

	// first thing is to retrieve the state and create a pool using the first
	// connectable connection. This connection is only used during
	// initialization, it gets closed right after
	{
		conn, err := sc.dialSentinel()
		if err != nil {
			return nil, err
		}
		defer conn.Close()

		if err := sc.ensureSentinelAddrs(conn); err != nil {
			return nil, err
		} else if err := sc.ensureClients(conn); err != nil {
			return nil, err
		}
	}

	// because we're using persistent these can't _really_ fail
	sc.pconn = PersistentPubSub("", "", func(_, _ string) (Conn, error) {
		return sc.dialSentinel()
	})
	sc.pconn.Subscribe(sc.pconnCh, "switch-master")

	sc.closeWG.Add(1)
	go sc.spin()
	return sc, nil
}

func (sc *Sentinel) err(err error) {
	select {
	case sc.ErrCh <- err:
	default:
	}
}

func (sc *Sentinel) testEvent(event string) {
	select {
	case sc.testEventCh <- event:
	default:
	}
}

func (sc *Sentinel) dialSentinel() (Conn, error) {
	sc.l.RLock()
	defer sc.l.RUnlock()

	var conn Conn
	var err error
	for addr := range sc.sentinelAddrs {
		conn, err = sc.so.cf("tcp", addr)
		if err == nil {
			return conn, nil
		}
	}

	// try the initAddrs as a last ditch, but don't return their error if this
	// doesn't work
	for _, addr := range sc.initAddrs {
		if conn, err := sc.so.cf("tcp", addr); err == nil {
			return conn, nil
		}
	}

	return nil, err
}

// Do implements the method for the Client interface. It will pass the given
// action on to the current primary.
//
// NOTE it's possible that in between Do being called and the Action being
// actually carried out that there could be a failover event. In that case, the
// Action will likely fail and return an error.
func (sc *Sentinel) Do(a Action) error {
	sc.l.RLock()
	defer sc.l.RUnlock()
	return sc.prim.Do(a)
}

// Addrs returns the currently known network address of the current primary
// instance and the addresses of the secondaries.
func (sc *Sentinel) Addrs() (string, []string) {
	sc.l.RLock()
	defer sc.l.RUnlock()
	secAddrs := make([]string, 0, len(sc.secsM))
	for addr := range sc.secsM {
		secAddrs = append(secAddrs, addr)
	}
	return sc.primAddr, secAddrs
}

// Client returns a Client for the given address, which could be either the
// primary or one of the secondaries (see Addrs method for retrieving known
// addresses).
//
// NOTE that if there is a failover while a Client returned by this method is
// being used the Client may or may not continue to work as expected, depending
// on the nature of the failover.
//
// NOTE the Client should _not_ be closed.
func (sc *Sentinel) Client(addr string) (Client, error) {
	sc.l.RLock()
	defer sc.l.RUnlock()
	if addr == sc.primAddr {
		return sc.prim, nil
	} else if _, ok := sc.secsM[addr]; ok {
		// TODO instead of creating an instance right here, keep track of
		// secondary Clients within Sentinel
		return sc.so.pf("tcp", addr)
	}
	return nil, errUnknownAddress
}

// Close implements the method for the Client interface.
func (sc *Sentinel) Close() error {
	sc.l.Lock()
	defer sc.l.Unlock()
	closeErr := errClientClosed
	sc.closeOnce.Do(func() {
		close(sc.closeCh)
		sc.closeWG.Wait()
		closeErr = sc.prim.Close()
	})
	return closeErr
}

// cmd should be the command called which generated m
func sentinelMtoAddr(m map[string]string, cmd string) (string, error) {
	if m["ip"] == "" || m["port"] == "" {
		return "", fmt.Errorf("malformed %s response", cmd)
	}
	return net.JoinHostPort(m["ip"], m["port"]), nil
}

// returns added/removed
func (sc *Sentinel) secsDiff(
	oldSecs, newSecs map[string]bool,
) (
	map[string]bool, map[string]bool,
) {
	removed := map[string]bool{}
	for addr := range oldSecs {
		if _, ok := newSecs[addr]; !ok {
			removed[addr] = true
		}
	}
	added := map[string]bool{}
	for addr := range newSecs {
		if _, ok := oldSecs[addr]; !ok {
			added[addr] = true
		}
	}
	return added, removed
}

// given a connection to a sentinel, ensures that the Clients currently being
// held agrees with what the sentinel thinks they should be
func (sc *Sentinel) ensureClients(conn Conn) error {
	// TODO it'd be ideal to pipeline these two commands, but doing so breaks
	// test since stub doesn't properly handle pipelining
	var primM map[string]string
	var secMM []map[string]string
	if err := conn.Do(Cmd(&primM, "SENTINEL", "MASTER", sc.name)); err != nil {
		return err
	} else if err := conn.Do(Cmd(&secMM, "SENTINEL", "SLAVES", sc.name)); err != nil {
		return err
	}

	newPrimAddr, err := sentinelMtoAddr(primM, "SENTINEL MASTER")
	if err != nil {
		return err
	}

	newSecsM := map[string]bool{}
	for _, secM := range secMM {
		newSecAddr, err := sentinelMtoAddr(secM, "SENTINEL SLAVES")
		if err != nil {
			return err
		}
		newSecsM[newSecAddr] = true
	}

	return sc.setClients(newPrimAddr, newSecsM)
}

// one day we might keep track of the secondary clients in here, not sure yet
func (sc *Sentinel) setClients(newPrimAddr string, newSecsM map[string]bool) error {
	sc.l.RLock()
	oldPrimAddr := sc.primAddr
	secsAdded, secsRemoved := sc.secsDiff(sc.secsM, newSecsM)
	sc.l.RUnlock()

	primChanged := oldPrimAddr != newPrimAddr
	if !primChanged && len(secsAdded) == 0 && len(secsRemoved) == 0 {
		return nil

	} else if !primChanged {
		sc.l.Lock()
		sc.secsM = newSecsM
		sc.l.Unlock()
		return nil
	}

	newPool, err := sc.so.pf("tcp", newPrimAddr)
	if err != nil {
		return err
	}

	sc.l.Lock()
	if sc.prim != nil {
		sc.prim.Close()
	}
	sc.prim = newPool
	sc.primAddr = newPrimAddr
	sc.secsM = newSecsM
	sc.l.Unlock()

	return nil
}

// annoyingly the SENTINEL SENTINELS <name> command doesn't return _this_
// sentinel instance, only the others it knows about for that primary
func (sc *Sentinel) ensureSentinelAddrs(conn Conn) error {
	var mm []map[string]string
	err := conn.Do(Cmd(&mm, "SENTINEL", "SENTINELS", sc.name))
	if err != nil {
		return err
	}

	addrs := map[string]bool{conn.NetConn().RemoteAddr().String(): true}
	for _, m := range mm {
		addrs[net.JoinHostPort(m["ip"], m["port"])] = true
	}

	sc.l.Lock()
	sc.sentinelAddrs = addrs
	sc.l.Unlock()
	return nil
}

func (sc *Sentinel) spin() {
	defer sc.closeWG.Done()
	defer sc.pconn.Close()
	for {
		if err := sc.innerSpin(); err != nil {
			sc.err(err)
			// sleep a second so we don't end up in a tight loop
			time.Sleep(1 * time.Second)
		}
		// This also gets checked within innerSpin to short-circuit that, but
		// we also must check in here to short-circuit this
		select {
		case <-sc.closeCh:
			return
		default:
		}
	}
}

// makes connection to an address in sc.addrs and handles
// the sentinel until that connection goes bad.
//
// Things this handles:
// * Listening for switch-master events (from pconn, which has reconnect logic
//   external to this package)
// * Periodically re-ensuring that the list of sentinel addresses is up-to-date
// * Periodically re-checking the current primary, in case the switch-master was
//   missed somehow
func (sc *Sentinel) innerSpin() error {
	conn, err := sc.dialSentinel()
	if err != nil {
		return err
	}
	defer conn.Close()

	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()

	var switchMaster bool
	for {
		if err := sc.ensureSentinelAddrs(conn); err != nil {
			return err
		} else if err := sc.ensureClients(conn); err != nil {
			return err
		}
		sc.pconn.Ping()

		// the tests want to know when the client state has been updated due to
		// a switch-master event
		if switchMaster {
			sc.testEvent("switch-master completed")
			switchMaster = false
		}

		select {
		case <-tick.C:
			// loop
		case <-sc.pconnCh:
			switchMaster = true
			// loop
		case <-sc.closeCh:
			return nil
		}
	}
}
