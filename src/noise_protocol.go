package main

import (
	"errors"
	"golang.org/x/crypto/blake2s"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/poly1305"
	"sync"
)

const (
	HandshakeZeroed = iota
	HandshakeInitiationCreated
	HandshakeInitiationConsumed
	HandshakeResponseCreated
	HandshakeResponseConsumed
)

const (
	NoiseConstruction = "Noise_IKpsk2_25519_ChaChaPoly_BLAKE2s"
	WGIdentifier      = "WireGuard v1 zx2c4 Jason@zx2c4.com"
	WGLabelMAC1       = "mac1----"
	WGLabelCookie     = "cookie--"
)

const (
	MessageInitiationType     = 1
	MessageResponseType       = 2
	MessageCookieResponseType = 3
	MessageTransportType      = 4
)

/* Type is an 8-bit field, followed by 3 nul bytes,
 * by marshalling the messages in little-endian byteorder
 * we can treat these as a 32-bit int
 *
 */

type MessageInitiation struct {
	Type      uint32
	Sender    uint32
	Ephemeral NoisePublicKey
	Static    [NoisePublicKeySize + poly1305.TagSize]byte
	Timestamp [TAI64NSize + poly1305.TagSize]byte
	Mac1      [blake2s.Size128]byte
	Mac2      [blake2s.Size128]byte
}

type MessageResponse struct {
	Type      uint32
	Sender    uint32
	Reciever  uint32
	Ephemeral NoisePublicKey
	Empty     [poly1305.TagSize]byte
	Mac1      [blake2s.Size128]byte
	Mac2      [blake2s.Size128]byte
}

type MessageTransport struct {
	Type     uint32
	Reciever uint32
	Counter  uint64
	Content  []byte
}

type Handshake struct {
	state                   int
	mutex                   sync.Mutex
	hash                    [blake2s.Size]byte       // hash value
	chainKey                [blake2s.Size]byte       // chain key
	presharedKey            NoiseSymmetricKey        // psk
	localEphemeral          NoisePrivateKey          // ephemeral secret key
	localIndex              uint32                   // used to clear hash-table
	remoteIndex             uint32                   // index for sending
	remoteStatic            NoisePublicKey           // long term key
	remoteEphemeral         NoisePublicKey           // ephemeral public key
	precomputedStaticStatic [NoisePublicKeySize]byte // precomputed shared secret
	lastTimestamp           TAI64N
}

var (
	InitalChainKey [blake2s.Size]byte
	InitalHash     [blake2s.Size]byte
	ZeroNonce      [chacha20poly1305.NonceSize]byte
)

func init() {
	InitalChainKey = blake2s.Sum256([]byte(NoiseConstruction))
	InitalHash = blake2s.Sum256(append(InitalChainKey[:], []byte(WGIdentifier)...))
}

func mixKey(c [blake2s.Size]byte, data []byte) [blake2s.Size]byte {
	return KDF1(c[:], data)
}

func mixHash(h [blake2s.Size]byte, data []byte) [blake2s.Size]byte {
	return blake2s.Sum256(append(h[:], data...))
}

func (h *Handshake) mixHash(data []byte) {
	h.hash = mixHash(h.hash, data)
}

func (h *Handshake) mixKey(data []byte) {
	h.chainKey = mixKey(h.chainKey, data)
}

func (device *Device) CreateMessageInitiation(peer *Peer) (*MessageInitiation, error) {
	handshake := &peer.handshake
	handshake.mutex.Lock()
	defer handshake.mutex.Unlock()

	// create ephemeral key

	var err error
	handshake.chainKey = InitalChainKey
	handshake.hash = mixHash(InitalHash, handshake.remoteStatic[:])
	handshake.localEphemeral, err = newPrivateKey()
	if err != nil {
		return nil, err
	}

	device.indices.ClearIndex(handshake.localIndex)
	handshake.localIndex, err = device.indices.NewIndex(peer)

	// assign index

	var msg MessageInitiation

	msg.Type = MessageInitiationType
	msg.Ephemeral = handshake.localEphemeral.publicKey()

	if err != nil {
		return nil, err
	}

	msg.Sender = handshake.localIndex
	handshake.mixKey(msg.Ephemeral[:])
	handshake.mixHash(msg.Ephemeral[:])

	// encrypt static key

	func() {
		var key [chacha20poly1305.KeySize]byte
		ss := handshake.localEphemeral.sharedSecret(handshake.remoteStatic)
		handshake.chainKey, key = KDF2(handshake.chainKey[:], ss[:])
		aead, _ := chacha20poly1305.New(key[:])
		aead.Seal(msg.Static[:0], ZeroNonce[:], device.publicKey[:], handshake.hash[:])
	}()
	handshake.mixHash(msg.Static[:])

	// encrypt timestamp

	timestamp := Timestamp()
	func() {
		var key [chacha20poly1305.KeySize]byte
		handshake.chainKey, key = KDF2(
			handshake.chainKey[:],
			handshake.precomputedStaticStatic[:],
		)
		aead, _ := chacha20poly1305.New(key[:])
		aead.Seal(msg.Timestamp[:0], ZeroNonce[:], timestamp[:], handshake.hash[:])
	}()

	handshake.mixHash(msg.Timestamp[:])
	handshake.state = HandshakeInitiationCreated

	return &msg, nil
}

func (device *Device) ConsumeMessageInitiation(msg *MessageInitiation) *Peer {
	if msg.Type != MessageInitiationType {
		return nil
	}

	hash := mixHash(InitalHash, device.publicKey[:])
	hash = mixHash(hash, msg.Ephemeral[:])
	chainKey := mixKey(InitalChainKey, msg.Ephemeral[:])

	// decrypt static key

	var err error
	var peerPK NoisePublicKey
	func() {
		var key [chacha20poly1305.KeySize]byte
		ss := device.privateKey.sharedSecret(msg.Ephemeral)
		chainKey, key = KDF2(chainKey[:], ss[:])
		aead, _ := chacha20poly1305.New(key[:])
		_, err = aead.Open(peerPK[:0], ZeroNonce[:], msg.Static[:], hash[:])
	}()
	if err != nil {
		return nil
	}
	hash = mixHash(hash, msg.Static[:])

	// find peer

	peer := device.LookupPeer(peerPK)
	if peer == nil {
		return nil
	}
	handshake := &peer.handshake
	handshake.mutex.Lock()
	defer handshake.mutex.Unlock()

	// decrypt timestamp

	var timestamp TAI64N
	func() {
		var key [chacha20poly1305.KeySize]byte
		chainKey, key = KDF2(
			chainKey[:],
			handshake.precomputedStaticStatic[:],
		)
		aead, _ := chacha20poly1305.New(key[:])
		_, err = aead.Open(timestamp[:0], ZeroNonce[:], msg.Timestamp[:], hash[:])
	}()
	if err != nil {
		return nil
	}
	hash = mixHash(hash, msg.Timestamp[:])

	// check for replay attack

	if !timestamp.After(handshake.lastTimestamp) {
		return nil
	}

	// TODO: check for flood attack

	// update handshake state

	handshake.hash = hash
	handshake.chainKey = chainKey
	handshake.remoteIndex = msg.Sender
	handshake.remoteEphemeral = msg.Ephemeral
	handshake.lastTimestamp = timestamp
	handshake.state = HandshakeInitiationConsumed
	return peer
}

func (device *Device) CreateMessageResponse(peer *Peer) (*MessageResponse, error) {
	handshake := &peer.handshake
	handshake.mutex.Lock()
	defer handshake.mutex.Unlock()

	if handshake.state != HandshakeInitiationConsumed {
		return nil, errors.New("handshake initation must be consumed first")
	}

	// assign index

	var err error
	device.indices.ClearIndex(handshake.localIndex)
	handshake.localIndex, err = device.indices.NewIndex(peer)
	if err != nil {
		return nil, err
	}

	var msg MessageResponse
	msg.Type = MessageResponseType
	msg.Sender = handshake.localIndex
	msg.Reciever = handshake.remoteIndex

	// create ephemeral key

	handshake.localEphemeral, err = newPrivateKey()
	if err != nil {
		return nil, err
	}
	msg.Ephemeral = handshake.localEphemeral.publicKey()
	handshake.mixHash(msg.Ephemeral[:])

	func() {
		ss := handshake.localEphemeral.sharedSecret(handshake.remoteEphemeral)
		handshake.mixKey(ss[:])
		ss = handshake.localEphemeral.sharedSecret(handshake.remoteStatic)
		handshake.mixKey(ss[:])
	}()

	// add preshared key (psk)

	var tau [blake2s.Size]byte
	var key [chacha20poly1305.KeySize]byte
	handshake.chainKey, tau, key = KDF3(handshake.chainKey[:], handshake.presharedKey[:])
	handshake.mixHash(tau[:])

	func() {
		aead, _ := chacha20poly1305.New(key[:])
		aead.Seal(msg.Empty[:0], ZeroNonce[:], nil, handshake.hash[:])
		handshake.mixHash(msg.Empty[:])
	}()

	handshake.state = HandshakeResponseCreated
	return &msg, nil
}

func (device *Device) ConsumeMessageResponse(msg *MessageResponse) *Peer {
	if msg.Type != MessageResponseType {
		return nil
	}

	// lookup handshake by reciever

	lookup := device.indices.Lookup(msg.Reciever)
	handshake := lookup.handshake
	if handshake == nil {
		return nil
	}

	handshake.mutex.Lock()
	defer handshake.mutex.Unlock()
	if handshake.state != HandshakeInitiationCreated {
		return nil
	}

	// finish 3-way DH

	hash := mixHash(handshake.hash, msg.Ephemeral[:])
	chainKey := handshake.chainKey

	func() {
		ss := handshake.localEphemeral.sharedSecret(msg.Ephemeral)
		chainKey = mixKey(chainKey, ss[:])
		ss = device.privateKey.sharedSecret(msg.Ephemeral)
		chainKey = mixKey(chainKey, ss[:])
	}()

	// add preshared key (psk)

	var tau [blake2s.Size]byte
	var key [chacha20poly1305.KeySize]byte
	chainKey, tau, key = KDF3(chainKey[:], handshake.presharedKey[:])
	hash = mixHash(hash, tau[:])

	// authenticate

	aead, _ := chacha20poly1305.New(key[:])
	_, err := aead.Open(nil, ZeroNonce[:], msg.Empty[:], hash[:])
	if err != nil {
		return nil
	}
	hash = mixHash(hash, msg.Empty[:])

	// update handshake state

	handshake.hash = hash
	handshake.chainKey = chainKey
	handshake.remoteIndex = msg.Sender
	handshake.state = HandshakeResponseConsumed

	return lookup.peer
}

func (peer *Peer) NewKeyPair() *KeyPair {
	handshake := &peer.handshake
	handshake.mutex.Lock()
	defer handshake.mutex.Unlock()

	// derive keys

	var isInitiator bool
	var sendKey [chacha20poly1305.KeySize]byte
	var recvKey [chacha20poly1305.KeySize]byte

	if handshake.state == HandshakeResponseConsumed {
		sendKey, recvKey = KDF2(handshake.chainKey[:], nil)
		isInitiator = true
	} else if handshake.state == HandshakeResponseCreated {
		recvKey, sendKey = KDF2(handshake.chainKey[:], nil)
		isInitiator = false
	} else {
		return nil
	}

	// create AEAD instances

	var keyPair KeyPair

	keyPair.send, _ = chacha20poly1305.New(sendKey[:])
	keyPair.recv, _ = chacha20poly1305.New(recvKey[:])
	keyPair.sendNonce = 0
	keyPair.recvNonce = 0

	// remap index

	peer.device.indices.Insert(handshake.localIndex, IndexTableEntry{
		peer:      peer,
		keyPair:   &keyPair,
		handshake: nil,
	})
	handshake.localIndex = 0

	// rotate key pairs

	func() {
		kp := &peer.keyPairs
		kp.mutex.Lock()
		defer kp.mutex.Unlock()
		if isInitiator {
			kp.previous = peer.keyPairs.current
			kp.current = &keyPair
			kp.newKeyPair <- true
		} else {
			kp.next = &keyPair
		}
	}()

	// zero handshake

	handshake.chainKey = [blake2s.Size]byte{}
	handshake.localEphemeral = NoisePrivateKey{}
	peer.handshake.state = HandshakeZeroed
	return &keyPair
}