package node

import (
	"context"
	"errors"
	"fmt"

	//"log/syslog"
	//"strconv"
	"sync"
	"time"

	proto "github.com/golang/protobuf/proto"
	logging "github.com/ipfs/go-log"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/p2p/protocol/identify"

	"github.com/libp2p/go-libp2p-core/event"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/peerstore"
	p2pproto "github.com/libp2p/go-libp2p-core/protocol"
	"github.com/libp2p/go-libp2p/p2p/protocol/ping"
	ma "github.com/multiformats/go-multiaddr"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"

	rendezvous "github.com/status-im/go-libp2p-rendezvous"
	"github.com/status-im/go-waku/waku/v2/metrics"
	"github.com/status-im/go-waku/waku/v2/protocol"
	"github.com/status-im/go-waku/waku/v2/protocol/filter"
	"github.com/status-im/go-waku/waku/v2/protocol/lightpush"
	"github.com/status-im/go-waku/waku/v2/protocol/pb"
	"github.com/status-im/go-waku/waku/v2/protocol/relay"
	"github.com/status-im/go-waku/waku/v2/protocol/store"
	wakurelay "github.com/status-im/go-wakurelay-pubsub"
)

var log = logging.Logger("wakunode")

//var logwriter, _ = syslog.New(syslog.LOG_ERR|syslog.LOG_LOCAL0, "WAKU")

type Message []byte

// A map of peer IDs to supported protocols
type PeerStats map[peer.ID][]string

type ConnStatus struct {
	IsOnline   bool
	HasHistory bool
	Peers      PeerStats
}

type WakuNode struct {
	host      host.Host
	idService *identify.IDService
	opts      *WakuNodeParameters

	relay     *relay.WakuRelay
	filter    *filter.WakuFilter
	lightPush *lightpush.WakuLightPush

	rendezvous *rendezvous.RendezvousService

	ping *ping.PingService

	subscriptions      map[relay.Topic][]*Subscription
	subscriptionsMutex sync.Mutex

	bcaster Broadcaster

	filters filter.Filters

	connectednessEventSub  event.Subscription
	protocolEventSub       event.Subscription
	identificationEventSub event.Subscription

	ctx    context.Context
	cancel context.CancelFunc
	quit   chan struct{}

	// Map of peers and their supported protocols
	peers      PeerStats
	peersMutex sync.Mutex

	// Internal protocol implementations that wish
	// to listen to peer added/removed events (e.g. Filter)
	peerListeners []chan *event.EvtPeerConnectednessChanged
	// Channel passed to WakuNode constructor
	// receiving connection status notifications
	connStatusChan chan ConnStatus
	pingEventsChan chan interface{}
}

func (w *WakuNode) handleConnectednessChanged(ev event.EvtPeerConnectednessChanged) {
	log.Debug("### EvtPeerConnectednessChanged ", w.Host().ID(), " to ", ev.Peer, " : ", ev.Connectedness)

	w.peersMutex.Lock()
	defer w.peersMutex.Unlock()

	if ev.Connectedness == network.Connected {
		_, ok := w.peers[ev.Peer]
		if !ok {
			peerProtocols, _ := w.host.Peerstore().GetProtocols(ev.Peer)
			log.Debug("protocols found for peer: ", ev.Peer, ", protocols: ", peerProtocols)
			w.peers[ev.Peer] = peerProtocols
		} else {
			log.Debug("### Peer already exists")
		}
	} else if ev.Connectedness == network.NotConnected {
		log.Debug("Peer down: ", ev.Peer)
		delete(w.peers, ev.Peer)
		// for _, pl := range w.peerListeners {
		// 	pl <- &ev
		// }
		// TODO
		// There seems to be no proper way to
		// remove a dropped peer from Host's Peerstore
		// https://github.com/libp2p/go-libp2p-host/issues/13
		//w.Host().Network().ClosePeer(ev.Peer)
	}

}
func (w *WakuNode) handleProtocolsUpdated(ev event.EvtPeerProtocolsUpdated) {
	log.Debug("### EvtPeerProtocolsUpdated ", w.Host().ID(), " to ", ev.Peer, " added: ", ev.Added, ", removed: ", ev.Removed)

	w.peersMutex.Lock()
	defer w.peersMutex.Unlock()

	_, ok := w.peers[ev.Peer]
	if ok {
		peerProtocols, _ := w.host.Peerstore().GetProtocols(ev.Peer)
		log.Debug("updated protocols found for peer: ", ev.Peer, ", protocols: ", peerProtocols)
		w.peers[ev.Peer] = peerProtocols
	}

}
func (w *WakuNode) handlePeerIdentificationCompleted(ev event.EvtPeerIdentificationCompleted) {
	log.Debug("### EvtPeerIdentificationCompleted ", w.Host().ID(), " to ", ev.Peer)

	w.peersMutex.Lock()
	defer w.peersMutex.Unlock()

	peerProtocols, _ := w.host.Peerstore().GetProtocols(ev.Peer)
	log.Debug("identified protocols found for peer: ", ev.Peer, ", protocols: ", peerProtocols)
	w.peers[ev.Peer] = peerProtocols
}
func (w *WakuNode) processHostEvent(e interface{}) {
	if e == nil {
		log.Debug("processHostEvent nil event")
		return
	}
	isOnline := w.IsOnline()
	hasHistory := w.HasHistory()
	switch e := e.(type) {
	case event.EvtPeerConnectednessChanged:
		w.handleConnectednessChanged(e)
	case event.EvtPeerProtocolsUpdated:
		w.handleProtocolsUpdated(e)
	case event.EvtPeerIdentificationCompleted:
		w.handlePeerIdentificationCompleted(e)
	}

	log.Debug("###processHostEvent before isOnline()")
	newIsOnline := w.IsOnline()
	log.Debug("###processHostEvent before hasHistory()")
	newHasHistory := w.HasHistory()
	log.Debug("###ConnStatus isOnline: ", isOnline, "/", newIsOnline, " hasHistory: ",
		hasHistory, "/", newHasHistory)
	if w.connStatusChan != nil {
		connStatus := ConnStatus{IsOnline: newIsOnline, HasHistory: newHasHistory, Peers: w.Peers()}
		log.Debug("New ConnStatus: ", connStatus)
		w.connStatusChan <- connStatus
	}

}
func (w *WakuNode) connectednessListener() {
	for {
		var e interface{}
		log.Debug("connectednessListener before select")
		select {
		case e = <-w.connectednessEventSub.Out():
			log.Debug("connectednessListener connectednessEvent")
		case e = <-w.protocolEventSub.Out():
			log.Debug("connectednessListener protocolEvent")
		case e = <-w.identificationEventSub.Out():
			log.Debug("connectednessListener identificationEvent")
		case e = <-w.pingEventsChan:
			log.Debug("connectednessListener pingEvent")
		}
		log.Debug("connectednessListener after select")

		w.processHostEvent(e)
		log.Debug("connectednessListener after processHostEvent")
	}
}

func New(ctx context.Context, opts ...WakuNodeOption) (*WakuNode, error) {
	params := new(WakuNodeParameters)

	ctx, cancel := context.WithCancel(ctx)

	params.libP2POpts = DefaultLibP2POptions

	for _, opt := range opts {
		err := opt(params)
		if err != nil {
			cancel()
			return nil, err
		}
	}

	if len(params.multiAddr) > 0 {
		params.libP2POpts = append(params.libP2POpts, libp2p.ListenAddrs(params.multiAddr...))
	}

	if params.privKey != nil {
		params.libP2POpts = append(params.libP2POpts, libp2p.Identity(*params.privKey))
	}

	host, err := libp2p.New(ctx, params.libP2POpts...)
	if err != nil {
		cancel()
		return nil, err
	}

	w := new(WakuNode)
	w.bcaster = NewBroadcaster(1024)
	w.host = host
	w.idService = identify.NewIDService(host)
	w.cancel = cancel
	w.ctx = ctx
	w.subscriptions = make(map[relay.Topic][]*Subscription)
	w.opts = params
	w.quit = make(chan struct{})
	w.peers = make(PeerStats)

	// Subscribe to Connectedness events
	connectednessEventSub, _ := host.EventBus().Subscribe(new(event.EvtPeerConnectednessChanged))
	w.connectednessEventSub = connectednessEventSub

	protocolEventSub, _ := host.EventBus().Subscribe(new(event.EvtPeerProtocolsUpdated))
	w.protocolEventSub = protocolEventSub

	identificationEventSub, _ := host.EventBus().Subscribe(new(event.EvtPeerIdentificationCompleted))
	w.identificationEventSub = identificationEventSub

	if params.connStatusChan != nil {
		w.connStatusChan = params.connStatusChan
	}
	w.pingEventsChan = make(chan interface{})
	go w.connectednessListener()

	if params.enableStore {
		w.startStore()
	}

	if params.enableFilter {
		w.filters = make(filter.Filters)
		err := w.mountFilter()
		if err != nil {
			return nil, err
		}
	}

	if params.enableRendezvous {
		rendezvous := rendezvous.NewRendezvousDiscovery(w.host, params.rendezvousPeers)
		params.wOpts = append(params.wOpts, wakurelay.WithDiscovery(rendezvous, params.rendezvousOpts...))
	}

	err = w.mountRelay(params.enableRelay, params.wOpts...)
	if err != nil {
		return nil, err
	}

	if params.enableLightPush {
		w.mountLightPush()
	}

	if params.keepAliveInterval > time.Duration(0) {
		w.startKeepAlive(params.keepAliveInterval)
	}

	if params.enableRendezvousServer {
		err := w.mountRendezvous()
		if err != nil {
			return nil, err
		}
	}

	for _, addr := range w.ListenAddresses() {
		log.Info("Listening on ", addr)
	}

	return w, nil
}

func (w *WakuNode) Stop() {
	w.subscriptionsMutex.Lock()
	defer w.subscriptionsMutex.Unlock()
	defer w.cancel()

	close(w.quit)

	defer w.connectednessEventSub.Close()
	defer w.protocolEventSub.Close()
	defer w.identificationEventSub.Close()

	if w.rendezvous != nil {
		w.rendezvous.Stop()
	}

	for _, topic := range w.relay.Topics() {
		for _, sub := range w.subscriptions[topic] {
			sub.Unsubscribe()
		}
	}

	w.subscriptions = nil

	w.host.Close()
}

func (w *WakuNode) Host() host.Host {
	return w.host
}

func (w *WakuNode) ID() string {
	return w.host.ID().Pretty()
}

func (w *WakuNode) IsOnline() bool {
	w.peersMutex.Lock()
	defer w.peersMutex.Unlock()

	hasRelay := false
	hasLightPush := false
	hasStore := false
	hasFilter := false
	for _, v := range w.peers {
		for _, protocol := range v {
			if !hasRelay && protocol == string(wakurelay.WakuRelayID_v200) {
				hasRelay = true
			}
			if !hasLightPush && protocol == string(lightpush.WakuLightPushProtocolId) {
				hasLightPush = true
			}
			if !hasStore && protocol == string(store.WakuStoreProtocolId) {
				hasStore = true
			}
			if !hasFilter && protocol == string(filter.WakuFilterProtocolId) {
				hasFilter = true
			}
			if hasRelay || hasLightPush && (hasStore || hasFilter) {
				return true
			}
		}
	}

	return false
}

func (w *WakuNode) HasHistory() bool {
	w.peersMutex.Lock()
	defer w.peersMutex.Unlock()

	for _, v := range w.peers {
		for _, protocol := range v {
			if protocol == string(store.WakuStoreProtocolId) {
				return true
			}
		}
	}
	return false
}

func (w *WakuNode) ListenAddresses() []ma.Multiaddr {
	hostInfo, _ := ma.NewMultiaddr(fmt.Sprintf("/p2p/%s", w.host.ID().Pretty()))
	var result []ma.Multiaddr
	for _, addr := range w.host.Addrs() {
		result = append(result, addr.Encapsulate(hostInfo))
	}
	return result
}

func (w *WakuNode) Relay() *relay.WakuRelay {
	return w.relay
}

func (w *WakuNode) Filter() *filter.WakuFilter {
	return w.filter
}

func (w *WakuNode) mountRelay(shouldRelayMessages bool, opts ...wakurelay.Option) error {
	var err error
	w.relay, err = relay.NewWakuRelay(w.ctx, w.host, opts...)

	if shouldRelayMessages {
		_, err := w.Subscribe(nil)
		if err != nil {
			return err
		}
	}

	// TODO: rlnRelay

	return err
}

func (w *WakuNode) mountFilter() error {
	filterHandler := func(requestId string, msg pb.MessagePush) {
		for _, message := range msg.Messages {
			w.filters.Notify(message, requestId) // Trigger filter handlers on a light node
		}
	}
	peerChan := make(chan *event.EvtPeerConnectednessChanged)
	w.filter = filter.NewWakuFilter(w.ctx, w.host, filterHandler, peerChan)
	w.peerListeners = append(w.peerListeners, peerChan)

	return nil

}
func (w *WakuNode) mountLightPush() {
	w.lightPush = lightpush.NewWakuLightPush(w.ctx, w.host, w.relay)
}

func (w *WakuNode) mountRendezvous() error {
	w.rendezvous = rendezvous.NewRendezvousService(w.host, w.opts.rendevousStorage)

	if err := w.rendezvous.Start(); err != nil {
		return err
	}

	log.Info("Rendezvous service started")
	return nil
}

func (w *WakuNode) AddPeer(info *peer.AddrInfo, protocolId string) error {
	log.Info(fmt.Sprintf("adding peer %s with protocol %s", info.ID.Pretty(), protocolId))

	w.host.Peerstore().AddAddrs(info.ID, info.Addrs, peerstore.PermanentAddrTTL)

	return w.host.Peerstore().AddProtocols(info.ID, protocolId)
}

func (w *WakuNode) startStore() {
	peerChan := make(chan *event.EvtPeerConnectednessChanged)
	w.opts.store.Start(w.ctx, w.host, peerChan)
	w.peerListeners = append(w.peerListeners, peerChan)

	if w.opts.shouldResume {
		if _, err := w.opts.store.Resume(string(relay.GetTopic(nil)), nil); err != nil {
			log.Error("failed to resume", err)
		}
	}
}

func (w *WakuNode) addPeerWithProtocol(address string, proto p2pproto.ID) (*peer.ID, error) {
	info, err := addrInfoFromMultiaddrString(address)
	if err != nil {
		return nil, err
	}

	return &info.ID, w.AddPeer(info, string(proto))
}

func (w *WakuNode) AddStorePeer(address string) (*peer.ID, error) {
	return w.addPeerWithProtocol(address, store.WakuStoreProtocolId)
}

func (w *WakuNode) AddRelayPeer(address string) (*peer.ID, error) {
	return w.addPeerWithProtocol(address, wakurelay.WakuRelayID_v200)
}

func (w *WakuNode) AddFilterPeer(address string) (*peer.ID, error) {
	return w.addPeerWithProtocol(address, filter.WakuFilterProtocolId)
}

func (w *WakuNode) AddLightPushPeer(address string) (*peer.ID, error) {
	return w.addPeerWithProtocol(address, lightpush.WakuLightPushProtocolId)
}

func (w *WakuNode) Query(ctx context.Context, contentTopics []string, startTime float64, endTime float64, opts ...store.HistoryRequestOption) (*pb.HistoryResponse, error) {
	if w.opts.store == nil {
		return nil, errors.New("WakuStore is not set")
	}

	query := new(pb.HistoryQuery)

	for _, ct := range contentTopics {
		query.ContentFilters = append(query.ContentFilters, &pb.ContentFilter{ContentTopic: ct})
	}

	query.StartTime = startTime
	query.EndTime = endTime
	query.PagingInfo = new(pb.PagingInfo)
	result, err := w.opts.store.Query(ctx, query, opts...)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (w *WakuNode) Resume(ctx context.Context, peerList []peer.ID) error {
	if w.opts.store == nil {
		return errors.New("WakuStore is not set")
	}

	result, err := w.opts.store.Resume(string(relay.DefaultWakuTopic), peerList)
	if err != nil {
		return err
	}

	log.Info("the number of retrieved messages since the last online time: ", result)

	return nil
}

func (node *WakuNode) Subscribe(topic *relay.Topic) (*Subscription, error) {
	// Subscribes to a PubSub topic.
	// NOTE The data field SHOULD be decoded as a WakuMessage.
	if node.relay == nil {
		return nil, errors.New("WakuRelay hasn't been set")
	}

	t := relay.GetTopic(topic)
	sub, isNew, err := node.relay.Subscribe(t)

	// Subscribe store to topic
	if isNew && node.opts.store != nil && node.opts.storeMsgs {
		log.Info("Subscribing store to topic ", t)
		node.bcaster.Register(node.opts.store.MsgC)
	}

	// Subscribe filter
	if isNew && node.filter != nil {
		log.Info("Subscribing filter to topic ", t)
		node.bcaster.Register(node.filter.MsgC)
	}

	if err != nil {
		return nil, err
	}

	// Create client subscription
	subscription := new(Subscription)
	subscription.closed = false
	subscription.C = make(chan *protocol.Envelope, 1024) // To avoid blocking
	subscription.quit = make(chan struct{})

	node.subscriptionsMutex.Lock()
	defer node.subscriptionsMutex.Unlock()

	node.subscriptions[t] = append(node.subscriptions[t], subscription)

	node.bcaster.Register(subscription.C)

	go func(t relay.Topic) {
		nextMsgTicker := time.NewTicker(time.Millisecond * 10)
		defer nextMsgTicker.Stop()

		ctx, err := tag.New(node.ctx, tag.Insert(metrics.KeyType, "relay"))
		if err != nil {
			log.Error(err)
			return
		}

		for {
			select {
			case <-subscription.quit:
				subscription.mutex.Lock()
				node.bcaster.Unregister(subscription.C) // Remove from broadcast list
				close(subscription.C)
				subscription.mutex.Unlock()
			case <-nextMsgTicker.C:
				msg, err := sub.Next(node.ctx)
				if err != nil {
					subscription.mutex.Lock()
					for _, subscription := range node.subscriptions[t] {
						subscription.Unsubscribe()
					}
					subscription.mutex.Unlock()
					return
				}

				stats.Record(ctx, metrics.Messages.M(1))

				wakuMessage := &pb.WakuMessage{}
				if err := proto.Unmarshal(msg.Data, wakuMessage); err != nil {
					log.Error("could not decode message", err)
					return
				}

				envelope := protocol.NewEnvelope(wakuMessage, string(t))

				node.bcaster.Submit(envelope)
			}
		}
	}(t)

	return subscription, nil
}

// Wrapper around WakuFilter.Subscribe
// that adds a Filter object to node.filters
func (node *WakuNode) SubscribeFilter(ctx context.Context, request pb.FilterRequest, ch filter.ContentFilterChan) error {
	// Registers for messages that match a specific filter. Triggers the handler whenever a message is received.
	// ContentFilterChan takes MessagePush structs

	// Status: Implemented.

	// Sanity check for well-formed subscribe FilterRequest
	//doAssert(request.subscribe, "invalid subscribe request")

	log.Info("SubscribeFilter, request: ", request)

	var id string
	var err error

	if node.filter == nil {
		return errors.New("WakuFilter is not set")
	}

	id, err = node.filter.Subscribe(ctx, request)
	if id == "" || err != nil {
		// Failed to subscribe
		log.Error("remote subscription to filter failed", request)
		//waku_node_errors.inc(labelValues = ["subscribe_filter_failure"])
		return err
	}

	// Register handler for filter, whether remote subscription succeeded or not
	node.filters[id] = filter.Filter{
		Topic:          request.Topic,
		ContentFilters: request.ContentFilters,
		Chan:           ch,
	}

	return nil
}

func (node *WakuNode) UnsubscribeFilter(ctx context.Context, request pb.FilterRequest) {

	log.Info("UnsubscribeFilter, request: ", request)
	// Send message to full node in order to unsubscribe
	node.filter.Unsubscribe(ctx, request)

	// Remove local filter
	var idsToRemove []string
	for id, f := range node.filters {
		// Iterate filter entries to remove matching content topics
		// make sure we delete the content filter
		// if no more topics are left
		for _, cfToDelete := range request.ContentFilters {
			for i, cf := range f.ContentFilters {
				if cf.ContentTopic == cfToDelete.ContentTopic {
					l := len(f.ContentFilters) - 1
					f.ContentFilters[l], f.ContentFilters[i] = f.ContentFilters[i], f.ContentFilters[l]
					f.ContentFilters = f.ContentFilters[:l]
					break
				}

			}
			if len(f.ContentFilters) == 0 {
				idsToRemove = append(idsToRemove, id)
			}
		}
	}

	for _, rId := range idsToRemove {
		for id := range node.filters {
			if id == rId {
				delete(node.filters, id)
				break
			}
		}
	}
}

func (node *WakuNode) Publish(ctx context.Context, message *pb.WakuMessage, topic *relay.Topic) ([]byte, error) {
	if node.relay == nil {
		return nil, errors.New("WakuRelay hasn't been set")
	}

	if message == nil {
		return nil, errors.New("message can't be null")
	}

	if node.lightPush != nil {
		return node.LightPush(ctx, message, topic)
	}

	hash, err := node.relay.Publish(ctx, message, topic)
	if err != nil {
		return nil, err
	}
	return hash, nil
}

func (node *WakuNode) LightPush(ctx context.Context, message *pb.WakuMessage, topic *relay.Topic, opts ...lightpush.LightPushOption) ([]byte, error) {
	if node.lightPush == nil {
		return nil, errors.New("WakuLightPush hasn't been set")
	}

	if message == nil {
		return nil, errors.New("message can't be null")
	}

	req := new(pb.PushRequest)
	req.Message = message
	req.PubsubTopic = string(relay.GetTopic(topic))

	response, err := node.lightPush.Request(ctx, req, opts...)
	if err != nil {
		return nil, err
	}

	if response.IsSuccess {
		hash, _ := message.Hash()
		return hash, nil
	} else {
		return nil, errors.New(response.Info)
	}
}

func (w *WakuNode) DialPeerWithMultiAddress(ctx context.Context, address ma.Multiaddr) error {
	info, err := peer.AddrInfoFromP2pAddr(address)
	if err != nil {
		return err
	}

	return w.connect(ctx, *info)
}

func (w *WakuNode) DialPeer(ctx context.Context, address string) error {
	p, err := ma.NewMultiaddr(address)
	if err != nil {
		return err
	}

	info, err := peer.AddrInfoFromP2pAddr(p)
	if err != nil {
		return err
	}

	return w.connect(ctx, *info)
}

func (w *WakuNode) connect(ctx context.Context, info peer.AddrInfo) error {
	err := w.host.Connect(ctx, info)
	if err != nil {
		return err
	}

	w.processHostEvent(event.EvtPeerConnectednessChanged{
		Peer:          info.ID,
		Connectedness: network.Connected,
	})

	return nil
}

func (w *WakuNode) DialPeerByID(ctx context.Context, peerID peer.ID) error {
	info := w.host.Peerstore().PeerInfo(peerID)
	return w.connect(ctx, info)
}

func (w *WakuNode) ClosePeerByAddress(address string) error {
	p, err := ma.NewMultiaddr(address)
	if err != nil {
		return err
	}

	// Extract the peer ID from the multiaddr.
	info, err := peer.AddrInfoFromP2pAddr(p)
	if err != nil {
		return err
	}

	return w.ClosePeerById(info.ID)
}

func (w *WakuNode) ClosePeerById(id peer.ID) error {
	err := w.host.Network().ClosePeer(id)
	if err != nil {
		return err
	}

	w.processHostEvent(event.EvtPeerConnectednessChanged{
		Peer:          id,
		Connectedness: network.NotConnected,
	})

	return nil
}

func (w *WakuNode) PeerCount() int {
	w.peersMutex.Lock()
	defer w.peersMutex.Unlock()
	return len(w.peers)
}

func (w *WakuNode) Peers() PeerStats {
	w.peersMutex.Lock()
	defer w.peersMutex.Unlock()
	p := make(PeerStats)
	for k, v := range w.peers {
		p[k] = v
	}

	return p
}

func (w *WakuNode) startKeepAlive(t time.Duration) {

	log.Info("Setting up ping protocol with duration of ", t)

	w.ping = ping.NewPingService(w.host)
	ticker := time.NewTicker(t)
	go func() {
		// This map contains peers that we're
		// waiting for the ping response from
		peerMap := make(map[peer.ID]<-chan ping.Result)
		var mu sync.Mutex
		for {
			select {
			case <-ticker.C:
				for _, p := range w.host.Peerstore().Peers() {
					if p == w.host.ID() {
						log.Info("###PING skip ", p)
						continue
					}
					mu.Lock()
					_, ok := peerMap[p]
					mu.Unlock()

					var s = p.Pretty()
					s = s[len(s)-4:]
					if !ok {
						log.Info("###PING ", s)
						result := w.ping.Ping(w.ctx, p)
						mu.Lock()
						peerMap[p] = result
						mu.Unlock()

						go func(peerID peer.ID) {
							peerFound := false
							w.peersMutex.Lock()
							for p := range w.peers {
								if p == peerID {
									peerFound = true
									break
								}
							}
							defer w.peersMutex.Unlock()
							log.Debug("###PING before fetching result")

							pingTicker := time.NewTicker(time.Duration(1) * time.Second)
							isError := false
							select {
							case resVal := <-result:
								isError = resVal.Error != nil
							case <-pingTicker.C:
								isError = true
							}
							pingTicker.Stop()
							if !peerFound && !isError {
								//EventBus Emitter doesn't seem to work when there's no connection
								w.pingEventsChan <- event.EvtPeerConnectednessChanged{
									Peer:          peerID,
									Connectedness: network.Connected,
								}
								peerConns := w.host.Network().ConnsToPeer(peerID)
								if len(peerConns) > 0 {
									// log.Info("###PING " + s + " IdentifyWait")
									// logwriter.Write([]byte("###PING " + s + " IdentifyWait"))
									//w.idService.IdentifyWait(peerConns[0])
								} else {
									go func(peerID peer.ID) {
										ctx, cancel := context.WithTimeout(w.ctx, time.Duration(5)*time.Second)
										defer cancel()
										if err := w.DialPeerByID(ctx, peerID); err != nil {
											log.Warn("could not dial peer ", peerID)
										}
									}(peerID)
								}
							} else if peerFound && isError {
								w.pingEventsChan <- event.EvtPeerConnectednessChanged{
									Peer:          peerID,
									Connectedness: network.NotConnected,
								}
							}

							mu.Lock()
							delete(peerMap, peerID)
							mu.Unlock()
						}(p)
					} else {
						log.Info("###PING " + s + " already pinged")
						// logwriter.Write([]byte("###PING " + s + " already pinged"))
					}
				}
			case <-w.quit:
				ticker.Stop()
				return
			}
		}
	}()
}

func addrInfoFromMultiaddrString(address string) (*peer.AddrInfo, error) {
	ma, err := ma.NewMultiaddr(address)
	if err != nil {
		return nil, err
	}

	return peer.AddrInfoFromP2pAddr(ma)
}
