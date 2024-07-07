package p2p

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/MixinNetwork/mixin/config"
	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/mixin/logger"
	"github.com/MixinNetwork/mixin/util"
	"github.com/dgraph-io/ristretto"
)

type Peer struct {
	IdForNetwork crypto.Hash
	Address      string

	sentMetric     *MetricPool
	receivedMetric *MetricPool

	ctx             context.Context
	handle          SyncHandle
	relayers        *neighborMap
	consumers       *neighborMap
	snapshotsCaches *confirmMap
	highRing        *util.RingBuffer
	normalRing      *util.RingBuffer
	syncRing        *util.RingBuffer
	closing         bool
	ops             chan struct{}
	stn             chan struct{}

	relayer        *QuicRelayer
	consumerAuth   *AuthToken
	isRelayer      bool
	remoteRelayers *relayersMap
}

type SyncPoint struct {
	NodeId crypto.Hash `json:"node"`
	Number uint64      `json:"round"`
	Hash   crypto.Hash `json:"hash"`
	Pool   any         `json:"pool"`
}

type ChanMsg struct {
	key  []byte
	data []byte
}

func (me *Peer) IsRelayer() bool {
	return me.isRelayer
}

func (me *Peer) ConnectRelayer(idForNetwork crypto.Hash, addr string) {
	if a, err := net.ResolveUDPAddr("udp", addr); err != nil {
		panic(fmt.Errorf("invalid address %s %s", addr, err))
	} else if a.Port < 80 || a.IP == nil {
		panic(fmt.Errorf("invalid address %s %d %s", addr, a.Port, a.IP))
	}
	if me.isRelayer {
		me.remoteRelayers = &relayersMap{m: make(map[crypto.Hash][]*remoteRelayer)}
	}

	for !me.closing {
		time.Sleep(time.Duration(config.SnapshotRoundGap))
		old := me.relayers.Get(idForNetwork)
		if old != nil {
			panic(fmt.Errorf("ConnectRelayer(%s) => %s", idForNetwork, old.Address))
		}
		relayer := NewPeer(nil, idForNetwork, addr, true)
		err := me.connectRelayer(relayer)
		logger.Printf("me.connectRelayer(%s, %v) => %v", me.Address, relayer, err)
	}
}

func (me *Peer) connectRelayer(relayer *Peer) error {
	logger.Printf("me.connectRelayer(%s, %s) => %v", me.Address, me.IdForNetwork, relayer)
	client, err := NewQuicConsumer(me.ctx, relayer.Address)
	logger.Printf("NewQuicConsumer(%s) => %v %v", relayer.Address, client, err)
	if err != nil {
		return err
	}
	defer client.Close("connectRelayer")
	defer relayer.disconnect()

	auth := me.handle.BuildAuthenticationMessage(relayer.IdForNetwork)
	err = client.Send(buildAuthenticationMessage(auth))
	logger.Printf("client.SendAuthenticationMessage(%x) => %v", auth, err)
	if err != nil {
		return err
	}
	me.sentMetric.handle(PeerMessageTypeAuthentication)
	if !me.relayers.Put(relayer.IdForNetwork, relayer) {
		panic(fmt.Errorf("ConnectRelayer(%s) => %s", relayer.IdForNetwork, relayer.Address))
	}
	defer me.relayers.Delete(relayer.IdForNetwork)

	go me.syncToNeighborLoop(relayer)
	go me.loopReceiveMessage(relayer, client)
	_, err = me.loopSendingStream(relayer, client)
	logger.Printf("me.loopSendingStream(%s, %s) => %v", me.Address, client.RemoteAddr().String(), err)
	return err
}

func (me *Peer) Neighbors() []*Peer {
	relayers := me.relayers.Slice()
	consumers := me.consumers.Slice()
	for _, c := range consumers {
		if slices.ContainsFunc(relayers, func(p *Peer) bool {
			return p.IdForNetwork == c.IdForNetwork
		}) {
			continue
		}
		relayers = append(relayers, c)
	}
	return relayers
}

func (p *Peer) disconnect() {
	if p.closing {
		return
	}
	p.closing = true
	p.highRing.Dispose()
	p.normalRing.Dispose()
	p.syncRing.Dispose()
	<-p.ops
	<-p.stn
}

func (me *Peer) Metric() map[string]*MetricPool {
	metrics := make(map[string]*MetricPool)
	if me.sentMetric.enabled {
		metrics["sent"] = me.sentMetric
	}
	if me.receivedMetric.enabled {
		metrics["received"] = me.receivedMetric
	}
	return metrics
}

func NewPeer(handle SyncHandle, idForNetwork crypto.Hash, addr string, isRelayer bool) *Peer {
	ringSize := uint64(1024)
	if isRelayer {
		ringSize = ringSize * MaxIncomingStreams
	}
	peer := &Peer{
		IdForNetwork:   idForNetwork,
		Address:        addr,
		relayers:       &neighborMap{m: make(map[crypto.Hash]*Peer)},
		consumers:      &neighborMap{m: make(map[crypto.Hash]*Peer)},
		highRing:       util.NewRingBuffer(ringSize),
		normalRing:     util.NewRingBuffer(ringSize),
		syncRing:       util.NewRingBuffer(ringSize),
		handle:         handle,
		sentMetric:     &MetricPool{enabled: false},
		receivedMetric: &MetricPool{enabled: false},
		ops:            make(chan struct{}),
		stn:            make(chan struct{}),
		isRelayer:      isRelayer,
	}
	peer.ctx = context.Background() // FIXME use real context
	if handle != nil {
		peer.snapshotsCaches = &confirmMap{cache: handle.GetCacheStore()}
	}
	return peer
}

func (me *Peer) Teardown() {
	me.closing = true
	if me.relayer != nil {
		me.relayer.Close()
	}
	me.highRing.Dispose()
	me.normalRing.Dispose()
	me.syncRing.Dispose()
	peers := me.Neighbors()
	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func(p *Peer) {
			p.disconnect()
			wg.Done()
		}(p)
	}
	wg.Wait()
	logger.Printf("Teardown(%s, %s)\n", me.IdForNetwork, me.Address)
}

func (me *Peer) ListenConsumers() error {
	logger.Printf("me.ListenConsumers(%s, %s)", me.Address, me.IdForNetwork)
	relayer, err := NewQuicRelayer(me.Address)
	if err != nil {
		return err
	}
	me.relayer = relayer
	me.remoteRelayers = &relayersMap{m: make(map[crypto.Hash][]*remoteRelayer)}

	go func() {
		ticker := time.NewTicker(time.Duration(config.SnapshotRoundGap))
		defer ticker.Stop()

		for !me.closing {
			neighbors := me.Neighbors()
			msg := me.buildConsumersMessage()
			for _, p := range neighbors {
				if !p.isRelayer {
					continue
				}
				key := crypto.Blake3Hash(append(msg, p.IdForNetwork[:]...))
				me.sendToPeer(p.IdForNetwork, PeerMessageTypeConsumers, key[:], msg, MsgPriorityNormal)
			}

			<-ticker.C
		}
	}()

	for !me.closing {
		c, err := me.relayer.Accept(me.ctx)
		logger.Printf("me.relayer.Accept(%s) => %v %v", me.Address, c, err)
		if err != nil {
			continue
		}
		go func(c Client) {
			defer c.Close("authenticateNeighbor")

			peer, err := me.authenticateNeighbor(c)
			logger.Printf("me.authenticateNeighbor(%s, %s) => %v %v", me.Address, c.RemoteAddr().String(), peer, err)
			if err != nil {
				return
			}
			defer peer.disconnect()

			old := me.consumers.Get(peer.IdForNetwork)
			if old != nil {
				old.disconnect()
				me.consumers.Delete(old.IdForNetwork)
			}
			if !me.consumers.Put(peer.IdForNetwork, peer) {
				panic(peer.IdForNetwork)
			}
			defer me.consumers.Delete(peer.IdForNetwork)

			go me.syncToNeighborLoop(peer)
			go me.loopReceiveMessage(peer, c)
			_, err = me.loopSendingStream(peer, c)
			logger.Printf("me.loopSendingStream(%s, %s) => %v", me.Address, c.RemoteAddr().String(), err)
		}(c)
	}

	logger.Printf("ListenConsumers(%s, %s) DONE\n", me.IdForNetwork, me.Address)
	return nil
}

func (me *Peer) loopSendingStream(p *Peer, consumer Client) (*ChanMsg, error) {
	defer close(p.ops)
	defer consumer.Close("loopSendingStream")

	for !me.closing && !p.closing {
		msgs := []*ChanMsg{}
		for len(msgs) < 16 {
			item, err := p.highRing.Poll(false)
			if err != nil {
				return nil, fmt.Errorf("peer.highRing(%s) => %v", p.IdForNetwork, err)
			} else if item == nil {
				break
			}
			msg := item.(*ChanMsg)
			if me.snapshotsCaches.contains(msg.key, time.Minute) {
				continue
			}
			msgs = append(msgs, msg)
		}

		for len(msgs) < 32 {
			item, err := p.normalRing.Poll(false)
			if err != nil {
				return nil, fmt.Errorf("peer.normalRing(%s) => %v", p.IdForNetwork, err)
			} else if item == nil {
				break
			}
			msg := item.(*ChanMsg)
			if me.snapshotsCaches.contains(msg.key, time.Minute) {
				continue
			}
			msgs = append(msgs, msg)
		}

		if len(msgs) == 0 {
			time.Sleep(300 * time.Millisecond)
			continue
		}

		for _, m := range msgs {
			err := consumer.Send(m.data)
			if err != nil {
				return m, fmt.Errorf("consumer.Send(%s, %d) => %v", p.Address, len(m.data), err)
			}
			if m.key != nil {
				me.snapshotsCaches.store(m.key, time.Now())
			}
		}
	}

	return nil, fmt.Errorf("PEER DONE")
}

func (me *Peer) loopReceiveMessage(peer *Peer, client Client) {
	logger.Printf("me.loopReceiveMessage(%s, %s)", me.Address, client.RemoteAddr().String())
	receive := make(chan *PeerMessage, 1024)
	defer close(receive)
	defer client.Close("loopReceiveMessage")

	go func() {
		defer client.Close("handlePeerMessage")

		for msg := range receive {
			err := me.handlePeerMessage(peer.IdForNetwork, msg)
			if err == nil {
				continue
			}
			logger.Printf("me.handlePeerMessage(%s) => %v", peer.IdForNetwork, err)
			return
		}
	}()

	for !me.closing {
		tm, err := client.Receive()
		if err != nil {
			logger.Printf("client.Receive %s %v", peer.Address, err)
			return
		}
		msg, err := parseNetworkMessage(tm.Version, tm.Data)
		if err != nil {
			logger.Debugf("parseNetworkMessage %s %v", peer.Address, err)
			return
		}
		me.receivedMetric.handle(msg.Type)

		select {
		case receive <- msg:
		default:
			logger.Printf("peer receive timeout %s", peer.Address)
			return
		}
	}
}

func (me *Peer) authenticateNeighbor(client Client) (*Peer, error) {
	var peer *Peer
	auth := make(chan error)
	go func() {
		tm, err := client.Receive()
		if err != nil {
			auth <- err
			return
		}
		msg, err := parseNetworkMessage(tm.Version, tm.Data)
		if err != nil {
			auth <- err
			return
		}
		if msg.Type != PeerMessageTypeAuthentication {
			auth <- fmt.Errorf("peer authentication invalid message type %d", msg.Type)
			return
		}
		me.receivedMetric.handle(PeerMessageTypeAuthentication)

		token, err := me.handle.AuthenticateAs(me.IdForNetwork, msg.Data, int64(HandshakeTimeout/time.Second))
		if err != nil {
			auth <- err
			return
		}

		addr := client.RemoteAddr().String()
		peer = NewPeer(nil, token.PeerId, addr, token.IsRelayer)
		peer.consumerAuth = token
		auth <- nil
	}()

	select {
	case err := <-auth:
		if err != nil {
			return nil, err
		}
	case <-time.After(3 * time.Second):
		return nil, fmt.Errorf("authenticate timeout")
	}
	return peer, nil
}

func (me *Peer) sendHighToPeer(to crypto.Hash, typ byte, key, data []byte) error {
	return me.sendToPeer(to, typ, key, data, MsgPriorityHigh)
}

func (me *Peer) offerWithCacheCheck(p *Peer, priority int, msg *ChanMsg) bool {
	if p.IdForNetwork == me.IdForNetwork {
		return true
	}
	if me.snapshotsCaches.contains(msg.key, time.Minute) {
		return true
	}
	return p.offer(priority, msg)
}

func (p *Peer) offer(priority int, msg *ChanMsg) bool {
	switch priority {
	case MsgPriorityNormal:
		s, err := p.normalRing.Offer(msg)
		return s && err == nil
	case MsgPriorityHigh:
		s, err := p.highRing.Offer(msg)
		return s && err == nil
	}
	panic(priority)
}

func (me *Peer) sendToPeer(to crypto.Hash, typ byte, key, data []byte, priority int) error {
	if to == me.IdForNetwork {
		return nil
	}
	if me.snapshotsCaches.contains(key, time.Minute) {
		return nil
	}
	me.sentMetric.handle(typ)

	peer := me.GetNeighbor(to)
	if peer != nil {
		success := peer.offer(priority, &ChanMsg{key, data})
		if !success {
			return fmt.Errorf("peer send %d timeout", priority)
		}
		return nil
	}

	rm := me.buildRelayMessage(to, data)
	rk := crypto.Blake3Hash(rm)
	rk = crypto.Blake3Hash(append(rk[:], []byte("REMOTE")...))
	relayers := me.GetRemoteRelayers(to)
	if len(relayers) == 0 {
		relayers = me.relayers.Slice()
	}
	for _, peer := range relayers {
		if !peer.IsRelayer() {
			panic(peer.IdForNetwork)
		}
		rk := crypto.Blake3Hash(append(rk[:], peer.IdForNetwork[:]...))
		success := peer.offer(priority, &ChanMsg{rk[:], rm})
		if !success {
			logger.Verbosef("peer.offer(%s) send timeout\n", peer.IdForNetwork)
		}
	}
	return nil
}

func (me *Peer) sendSnapshotMessageToPeer(to crypto.Hash, snap crypto.Hash, typ byte, data []byte) error {
	key := append(to[:], snap[:]...)
	key = append(key, 'S', 'N', 'A', 'P', typ)
	return me.sendToPeer(to, typ, key, data, MsgPriorityNormal)
}

type confirmMap struct {
	cache *ristretto.Cache
}

func (m *confirmMap) contains(key []byte, duration time.Duration) bool {
	if key == nil {
		return false
	}
	val, found := m.cache.Get(key)
	if found {
		ts := time.Unix(0, int64(binary.BigEndian.Uint64(val.([]byte))))
		return ts.Add(duration).After(time.Now())
	}
	return false
}

func (m *confirmMap) store(key []byte, ts time.Time) {
	if key == nil {
		panic(ts)
	}
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(ts.UnixNano()))
	m.cache.Set(key, buf, 8)
}

type remoteRelayer struct {
	Id       crypto.Hash
	ActiveAt time.Time
}

type relayersMap struct {
	sync.RWMutex
	m map[crypto.Hash][]*remoteRelayer
}

func (me *Peer) GetNeighbor(key crypto.Hash) *Peer {
	p := me.relayers.Get(key)
	if p != nil {
		return p
	}
	return me.consumers.Get(key)
}

func (me *Peer) GetRemoteRelayers(key crypto.Hash) []*Peer {
	if me.remoteRelayers == nil {
		return nil
	}
	var relayers []*Peer
	ids := me.remoteRelayers.Get(key)
	for _, id := range ids {
		p := me.GetNeighbor(id)
		if p != nil {
			relayers = append(relayers, p)
		}
	}
	return relayers
}

func (m *relayersMap) Get(key crypto.Hash) []crypto.Hash {
	m.RLock()
	defer m.RUnlock()

	var relayers []crypto.Hash
	for _, r := range m.m[key] {
		if r.ActiveAt.Add(time.Minute).Before(time.Now()) {
			continue
		}
		relayers = append(relayers, r.Id)
	}
	return relayers
}

func (m *relayersMap) Add(key crypto.Hash, v crypto.Hash) {
	m.Lock()
	defer m.Unlock()

	var relayers []*remoteRelayer
	for _, r := range m.m[key] {
		if r.ActiveAt.Add(time.Minute).After(time.Now()) {
			relayers = append(relayers, r)
		}
	}
	for _, r := range relayers {
		if r.Id == v {
			r.ActiveAt = time.Now()
			return
		}
	}
	i := slices.IndexFunc(relayers, func(r *remoteRelayer) bool {
		return r.Id == v
	})
	if i < 0 {
		relayers = append(relayers, &remoteRelayer{ActiveAt: time.Now(), Id: v})
	} else {
		relayers[i].ActiveAt = time.Now()
	}
	m.m[key] = relayers
}

type neighborMap struct {
	sync.RWMutex
	m map[crypto.Hash]*Peer
}

func (m *neighborMap) Get(key crypto.Hash) *Peer {
	m.RLock()
	defer m.RUnlock()

	return m.m[key]
}

func (m *neighborMap) Delete(key crypto.Hash) {
	m.Lock()
	defer m.Unlock()

	delete(m.m, key)
}

func (m *neighborMap) Set(key crypto.Hash, v *Peer) {
	m.Lock()
	defer m.Unlock()

	m.m[key] = v
}

func (m *neighborMap) Put(key crypto.Hash, v *Peer) bool {
	m.Lock()
	defer m.Unlock()

	if m.m[key] != nil {
		return false
	}
	m.m[key] = v
	return true
}

func (m *neighborMap) Slice() []*Peer {
	m.Lock()
	defer m.Unlock()

	var peers []*Peer
	for _, p := range m.m {
		peers = append(peers, p)
	}
	return peers
}

func (m *neighborMap) Clear() {
	m.Lock()
	defer m.Unlock()

	for id := range m.m {
		delete(m.m, id)
	}
}

func (m *neighborMap) RunOnce(key crypto.Hash, v *Peer, f func()) {
	m.Lock()
	defer m.Unlock()

	if m.m[key] != nil {
		return
	}
	m.m[key] = v
	go f()
}
