package discovery

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net"
	"strings"

	"github.com/davecgh/go-spew/spew"
	"github.com/samvrlewis/lnd/autopilot"
	"github.com/samvrlewis/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil/bech32"
)

// NetworkPeerBootstrapper is an interface that represents an initial peer
// boostrap mechanism. This interface is to be used to bootstrap a new peer to
// the connection by providing it with the pubkey+address of a set of existing
// peers on the network. Several bootstrap mechanisms can be implemented such
// as DNS, in channel graph, DHT's, etc.
type NetworkPeerBootstrapper interface {
	// SampleNodeAddrs uniformly samples a set of specified address from
	// the network peer bootstrapper source. The num addrs field passed in
	// denotes how many valid peer addresses to return. The passed set of
	// node nodes allows the caller to ignore a set of nodes perhaps
	// because they already have connections established.
	SampleNodeAddrs(numAddrs uint32,
		ignore map[autopilot.NodeID]struct{}) ([]*lnwire.NetAddress, error)

	// Name returns a human readable string which names the concrete
	// implementation of the NetworkPeerBootstrapper.
	Name() string
}

// MultiSourceBootstrap attempts to utilize a set of NetworkPeerBootstrapper
// passed in to return the target (numAddrs) number of peer addresses that can
// be used to bootstrap a peer just joining the Lightning Network. Each
// bootstrapper will be queried successively until the target amount is met. If
// the ignore map is populated, then the bootstrappers will be instructed to
// skip those nodes.
func MultiSourceBootstrap(ignore map[autopilot.NodeID]struct{}, numAddrs uint32,
	bootStrappers ...NetworkPeerBootstrapper) ([]*lnwire.NetAddress, error) {

	var addrs []*lnwire.NetAddress
	for _, bootStrapper := range bootStrappers {
		// If we already have enough addresses, then we can exit early
		// w/o querying the additional boostrappers.
		if uint32(len(addrs)) >= numAddrs {
			break
		}

		log.Infof("Attempting to bootstrap with: %v", bootStrapper.Name())

		// If we still need additional addresses, then we'll compute
		// the number of address remaining that we need to fetch.
		numAddrsLeft := numAddrs - uint32(len(addrs))
		log.Tracef("Querying for %v addresses", numAddrsLeft)
		netAddrs, err := bootStrapper.SampleNodeAddrs(numAddrsLeft, ignore)
		if err != nil {
			// If we encounter an error with a bootstrapper, then
			// we'll continue on to the next available
			// bootstrapper.
			log.Errorf("Unable to query bootstrapper %v: %v",
				bootStrapper.Name(), err)
			continue
		}

		addrs = append(addrs, netAddrs...)
	}

	log.Infof("Obtained %v addrs to bootstrap network with", len(addrs))

	return addrs, nil
}

// ChannelGraphBootstrapper is an implementation of the NetworkPeerBootstrapper
// which attempts to retrieve advertised peers directly from the active channel
// graph. This instance requires a backing autopilot.ChannelGraph instance in
// order to operate properly.
type ChannelGraphBootstrapper struct {
	chanGraph autopilot.ChannelGraph

	// hashAccumulator is a set of 32 random bytes that are read upon the
	// creation of the channel graph boostrapper. We use this value to
	// randomly select nodes within the known graph to connect to. After
	// each selection, we rotate the accumulator by hashing it with itself.
	hashAccumulator [32]byte

	tried map[autopilot.NodeID]struct{}
}

// A compile time assertion to ensure that ChannelGraphBootstrapper meets the
// NetworkPeerBootstrapper interface.
var _ NetworkPeerBootstrapper = (*ChannelGraphBootstrapper)(nil)

// NewGraphBootstrapper returns a new instance of a ChannelGraphBootstrapper
// backed by an active autopilot.ChannelGraph instance. This type of network
// peer bootstrapper will use the authenticated nodes within the known channel
// graph to bootstrap connections.
func NewGraphBootstrapper(cg autopilot.ChannelGraph) (NetworkPeerBootstrapper, error) {

	c := &ChannelGraphBootstrapper{
		chanGraph: cg,
		tried:     make(map[autopilot.NodeID]struct{}),
	}

	if _, err := rand.Read(c.hashAccumulator[:]); err != nil {
		return nil, err
	}

	return c, nil
}

// SampleNodeAddrs uniformly samples a set of specified address from the
// network peer bootstrapper source. The num addrs field passed in denotes how
// many valid peer addresses to return.
//
// NOTE: Part of the NetworkPeerBootstrapper interface.
func (c *ChannelGraphBootstrapper) SampleNodeAddrs(numAddrs uint32,
	ignore map[autopilot.NodeID]struct{}) ([]*lnwire.NetAddress, error) {

	// We'll merge the ignore map with our currently selected map in order
	// to ensure we don't return any duplicate nodes.
	for n := range ignore {
		c.tried[n] = struct{}{}
	}

	// In order to bootstrap, we'll iterate all the nodes in the channel
	// graph, accumulating nodes until either we go through all active
	// nodes, or we reach our limit. We ensure that we meet the randomly
	// sample constraint as we maintain an xor accumulator to ensure we
	// randomly sample nodes independent of the iteration of the channel
	// graph.
	sampleAddrs := func() ([]*lnwire.NetAddress, error) {
		var (
			a []*lnwire.NetAddress

			// We'll create a special error so we can return early
			// and abort the transaction once we find a match.
			errFound = fmt.Errorf("found node")
		)

		err := c.chanGraph.ForEachNode(func(node autopilot.Node) error {
			nID := autopilot.NewNodeID(node.PubKey())
			if _, ok := c.tried[nID]; ok {
				return nil
			}

			// We'll select the first node we come across who's
			// public key is less than our current accumulator
			// value. When comparing, we skip the first byte as
			// it's 50/50. If it isn't less, than then we'll
			// continue forward.
			nodePub := node.PubKey().SerializeCompressed()[1:]
			if bytes.Compare(c.hashAccumulator[:], nodePub) > 0 {
				return nil
			}

			for _, nodeAddr := range node.Addrs() {
				// If we haven't yet reached our limit, then
				// we'll copy over the details of this node
				// into the set of addresses to be returned.
				tcpAddr, ok := nodeAddr.(*net.TCPAddr)
				if !ok {
					// If this isn't a valid TCP address,
					// then we'll ignore it as currently
					// we'll only attempt to connect out to
					// TCP peers.
					return nil
				}

				// At this point, we've found an eligible node,
				// so we'll return early with our shibboleth
				// error.
				a = append(a, &lnwire.NetAddress{
					IdentityKey: node.PubKey(),
					Address:     tcpAddr,
				})
			}

			c.tried[nID] = struct{}{}

			return errFound
		})
		if err != nil && err != errFound {
			return nil, err
		}

		return a, nil
	}

	// We'll loop and sample new addresses from the graph source until
	// we've reached our target number of outbound connections or we hit 50
	// attempts, which ever comes first.
	var (
		addrs []*lnwire.NetAddress
		tries uint32
	)
	for tries < 30 && uint32(len(addrs)) < numAddrs {
		sampleAddrs, err := sampleAddrs()
		if err != nil {
			return nil, err
		}

		tries++

		// We'll now rotate our hash accumulator one value forwards.
		c.hashAccumulator = sha256.Sum256(c.hashAccumulator[:])

		// If this attempt didn't yield any addresses, then we'll exit
		// early.
		if len(sampleAddrs) == 0 {
			continue
		}

		addrs = append(addrs, sampleAddrs...)
	}

	log.Tracef("Ending hash accumulator state: %x", c.hashAccumulator)

	return addrs, nil
}

// Name returns a human readable string which names the concrete implementation
// of the NetworkPeerBootstrapper.
//
// NOTE: Part of the NetworkPeerBootstrapper interface.
func (c *ChannelGraphBootstrapper) Name() string {
	return "Authenticated Channel Graph"
}

// DNSSeedBootstrapper as an implementation of the NetworkPeerBootstrapper
// interface which implements peer bootstrapping via a spcial DNS seed as
// defined in BOLT-0010. For further details concerning Lightning's current DNS
// boot strapping protocol, see this link:
//     * https://github.com/samvrlewis/lightning-rfc/blob/master/10-dns-bootstrap.md
type DNSSeedBootstrapper struct {
	dnsSeeds []string
}

// A compile time assertion to ensure that DNSSeedBootstrapper meets the
// NetworkPeerjBootstrapper interface.
var _ NetworkPeerBootstrapper = (*ChannelGraphBootstrapper)(nil)

// NewDNSSeedBootstrapper returns a new instance of the DNSSeedBootstrapper.
// The set of passed seeds should point to DNS servers that properly implement
// Lighting's DNS peer bootstrapping protocol as defined in BOLT-0010.
//
//
// TODO(roasbeef): add a lookUpFunc param to pass in, so can divert queries
// over Tor in future
func NewDNSSeedBootstrapper(seeds []string) (NetworkPeerBootstrapper, error) {
	return &DNSSeedBootstrapper{
		dnsSeeds: seeds,
	}, nil
}

// SampleNodeAddrs uniformly samples a set of specified address from the
// network peer bootstrapper source. The num addrs field passed in denotes how
// many valid peer addresses to return. The set of DNS seeds are used
// successively to retrieve eligible target nodes.
func (d *DNSSeedBootstrapper) SampleNodeAddrs(numAddrs uint32,
	ignore map[autopilot.NodeID]struct{}) ([]*lnwire.NetAddress, error) {

	var netAddrs []*lnwire.NetAddress

	// We'll continue this loop until we reach our target address limit.
	// Each SRV query to the seed will return 25 random nodes, so we can
	// continue to query until we reach our target.
search:
	for uint32(len(netAddrs)) < numAddrs {
		for _, dnsSeed := range d.dnsSeeds {
			// We'll first query the seed with an SRV record so we
			// can obtain a random sample of the encoded public
			// keys of nodes.
			_, addrs, err := net.LookupSRV("nodes", "tcp", dnsSeed)
			if err != nil {
				return nil, err
			}

			log.Tracef("Retrieved SRV records from dns seed: %v",
				spew.Sdump(addrs))

			// Next, we'll need to issue an A record request for
			// each of the nodes, skipping it if nothing comes
			// back.
			for _, nodeSrv := range addrs {
				if uint32(len(netAddrs)) >= numAddrs {
					break search
				}

				// With the SRV target obtained, we'll now
				// perform another query to obtain the IP
				// address for the matching bech32 encoded node
				// key.
				bechNodeHost := nodeSrv.Target
				addrs, err := net.LookupHost(bechNodeHost)
				if err != nil {
					return nil, err
				}

				if len(addrs) == 0 {
					log.Tracef("No addresses for %v, skipping",
						bechNodeHost)
					continue
				}

				log.Tracef("Attempting to convert: %v", bechNodeHost)

				// If we have a set of valid addresses, then
				// we'll need to parse the public key from the
				// original bech32 encoded string.
				bechNode := strings.Split(bechNodeHost, ".")
				_, nodeBytes5Bits, err := bech32.Decode(bechNode[0])
				if err != nil {
					return nil, err
				}

				// Once we have the bech32 decoded pubkey,
				// we'll need to convert the 5-bit word
				// grouping into our regular 8-bit word
				// grouping so we can convert it into a public
				// key.
				nodeBytes, err := bech32.ConvertBits(
					nodeBytes5Bits, 5, 8, false,
				)
				if err != nil {
					return nil, err
				}
				nodeKey, err := btcec.ParsePubKey(
					nodeBytes, btcec.S256(),
				)
				if err != nil {
					return nil, err
				}

				// If we have an ignore list, and this node is
				// in the ignore list, then we'll go to the
				// next candidate.
				if ignore != nil {
					nID := autopilot.NewNodeID(nodeKey)
					if _, ok := ignore[nID]; ok {
						continue
					}
				}

				// Finally we'll convert the host:port peer to
				// a proper TCP address to use within the
				// lnwire.NetAddress.
				addr := fmt.Sprintf("%v:%v", addrs[0],
					nodeSrv.Port)
				tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
				if err != nil {
					return nil, err
				}

				// Finally, with all the information parsed,
				// we'll return this fully valid address as a
				// connection attempt.
				lnAddr := &lnwire.NetAddress{
					IdentityKey: nodeKey,
					Address:     tcpAddr,
				}

				log.Tracef("Obtained %v as valid reachable "+
					"node", lnAddr)

				netAddrs = append(netAddrs, lnAddr)
			}
		}
	}

	return netAddrs, nil
}

// Name returns a human readable string which names the concrete
// implementation of the NetworkPeerBootstrapper.
func (d *DNSSeedBootstrapper) Name() string {
	return fmt.Sprintf("BOLT-0010 DNS Seed: %v", d.dnsSeeds)
}
