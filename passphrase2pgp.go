// This is free and unencumbered software released into the public domain.

package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math/bits"
	"os"
	"syscall"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/ed25519"
	"golang.org/x/crypto/ssh/terminal"
)

const (
	kdfTime      = 8
	kdfMemory    = 1024 * 1024 // 1 GB
	pubKeyLen    = 53
	pubSubkeyLen = 58
)

// Returns data encoded as an OpenPGP multiprecision integer.
func mpi(data []byte) []byte {
	// Chop off leading zeros
	for len(data) > 0 && data[0] == 0 {
		data = data[1:]
	}
	// Zero-length is a special case (should never actually happen)
	if len(data) == 0 {
		return []byte{0, 0}
	}
	c := len(data)*8 - bits.LeadingZeros8(data[0])
	mpi := []byte{byte(c >> 8), byte(c >> 0)}
	return append(mpi, data...)
}

// Print the message like fmt.Printf() and then os.Exit(1).
func fatal(format string, args ...interface{}) {
	buf := bytes.NewBufferString("passphrase2pgp: ")
	fmt.Fprintf(buf, format, args...)
	buf.WriteRune('\n')
	os.Stderr.Write(buf.Bytes())
	os.Exit(1)
}

// Read, confirm, and return a passphrase from the user.
func readPassphrase(repeat int) ([]byte, error) {
	tail := []byte("\n")
	os.Stderr.Write([]byte("passphrase: "))
	passphrase, err := terminal.ReadPassword(int(syscall.Stdin))
	if err != nil {
		return nil, err
	}
	os.Stderr.Write(tail)
	for i := 0; i < repeat; i++ {
		os.Stderr.Write([]byte("passphrase (repeat): "))
		again, err := terminal.ReadPassword(int(syscall.Stdin))
		if err != nil {
			return nil, err
		}
		os.Stderr.Write(tail)
		if !bytes.Equal(again, passphrase) {
			return nil, errors.New("passphrases do not match")
		}
	}
	return passphrase, nil
}

// Returns the first line of a file not including \r or \n. Does not
// require a newline and does not return io.EOF.
func firstLine(filename string) ([]byte, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	if !s.Scan() {
		if err := s.Err(); err != io.EOF {
			return nil, err
		}
		return nil, nil // empty files are ok
	}
	return s.Bytes(), nil
}

// Derive a 64-byte seed from the given passphrase. The scale factor
// scales up the difficulty proportional to scale*scale.
func kdf(passphrase, uid []byte, scale int) []byte {
	var time uint32 = uint32(kdfTime * scale)
	var memory uint32 = uint32(kdfMemory * scale)
	var threads uint8 = 1
	return argon2.IDKey(passphrase, uid, time, memory, threads, 64)
}

// Returns a Secret-Key Packet for a key pair.
func newSecretKeyPacket(seckey, pubkey []byte, created int64) []byte {
	packet := []byte{
		0xc5, // packet header, new format, Secret-Key Packet (5)
		0,    // packet length
		0x04, // packet version, new (4)

		// Public Key
		// creation date
		byte(created >> 24),
		byte(created >> 16),
		byte(created >> 8),
		byte(created >> 0),
		22, // algorithm, EdDSA
		9,  // OID length
		// OID (1.3.6.1.4.1.11591.15.1)
		0x2b, 0x06, 0x01, 0x04, 0x01, 0xda, 0x47, 0x0f, 0x01,
		// public key length (always 263 bits)
		0x01, 0x07,
		0x40, // MPI prefix
		// public key (32 bytes)
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,

		// Secret Key
		0, // string-to-key, unencrypted
	}
	copy(packet[21:53], pubkey)

	// append MPI-encoded key
	secmpi := mpi(seckey)
	packet = append(packet, secmpi...)

	// compute and append checksum
	var checksum uint16
	for _, b := range secmpi {
		checksum += uint16(b)
	}
	packet = append(packet, []byte{
		byte(checksum >> 8), byte(checksum >> 0),
	}...)

	packet[1] = byte(len(packet) - 2)
	return packet
}

// Return a primary key and User ID loaded from a reader. Only keys
// previously output by passphrase2pgp are supported.
func load(r io.Reader) (key ed25519.PrivateKey, uid []byte, err error) {
	invalid := errors.New("invalid input key")
	defer func() {
		if recover() != nil {
			err = invalid
		}
	}()

	buf, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, nil, err
	}

	if buf[0] != 0xc5 {
		return nil, nil, invalid
	}
	plen := int(buf[1])
	skpacket := buf[:plen] // chop off checksum
	pubkey := skpacket[pubKeyLen-32 : pubKeyLen]

	seckey := skpacket[pubKeyLen+3:]
	seckey = append(make([]byte, 32-len(seckey)), seckey...)

	key = ed25519.NewKeyFromSeed(seckey)
	if !bytes.Equal(pubkey, key[32:]) {
		return nil, nil, invalid
	}

	buf = buf[plen+2:] // chop off Secret-Key Packet

	if buf[0] != 0xcd {
		return nil, nil, invalid
	}
	plen = int(buf[1])
	idpacket := buf[:plen+2]
	uid = idpacket[2:]

	return
}

// Returns a User ID Packet for the given identity.
func newUserIDPacket(uid string) []byte {
	return append([]byte{
		0xcd,           // packet header, new format, User ID Packet (13)
		byte(len(uid)), // packet length
	}, []byte(uid)...)
}

type signatureContext struct {
	key                           ed25519.PrivateKey
	skpacket, sskpacket, idpacket []byte
	created                       int64
	mdc                           bool
}

// Returns a Signature Packet binding a Secret-Key Packet and User ID Packet.
func signKey(ctx *signatureContext) []byte {
	keyid := keyid(ctx.skpacket)
	var sigType byte
	if ctx.sskpacket == nil {
		sigType = 0x13 // Positive certification of a User ID
	} else {
		sigType = 0x18 // Subkey Binding Signature
	}

	buf := bytes.NewBuffer([]byte{
		0xc2,    // packet header, new format, Signature Packet (2)
		0,       // packet length
		0x04,    // packet version, new (4)
		sigType, // signature type
		22,      // public-key algorithm, EdDSA
		8,       // hash algorithm, SHA-256
		0, 0,    // hashed subpacket data length
	})

	// Signature Creation Time subpacket (length=5, type=2)
	buf.Write([]byte{
		5, 2,
		byte(ctx.created >> 24),
		byte(ctx.created >> 16),
		byte(ctx.created >> 8),
		byte(ctx.created >> 0),
	})

	// Issuer subpacket (length=9, type=16)
	buf.Write([]byte{
		9, 16,
	})
	buf.Write(keyid[12:])
	// An Issuer Fingerprint subpacket is unnecessary here because this
	// is a self-signature, and so even the Issuer subpacket is already
	// redundant. The recipient already knows which key we're talking
	// about. Technically the Issuer subpacket is optional, but GnuPG
	// will not import a key without it.

	if ctx.mdc {
		// Features subpacket
		buf.Write([]byte{
			2, 30,
			0x01, // MDC
		})
	}

	// Actual hashed subpacket data length
	hashedLen := buf.Len() - 8

	// Unhashed subpacket data (none)
	buf.Write([]byte{
		0, 0,
	})

	// Fill out hashed data length
	sigpacket := buf.Bytes()
	binary.BigEndian.PutUint16(sigpacket[6:], uint16(hashedLen))

	// Compute digest to be signed
	h := sha256.New()
	h.Write([]byte{0x99, 0, 51})
	h.Write(ctx.skpacket[2:pubKeyLen]) // public key portion
	if sigType == 0x13 {
		// Secret-Key signature
		h.Write([]byte{0xb4, 0, 0, 0})
		h.Write(ctx.idpacket[1:])
	} else {
		// Secret-Subkey signature
		h.Write([]byte{0x99, 0, 56})
		h.Write(ctx.sskpacket[2:pubSubkeyLen])
	}
	h.Write(sigpacket[2 : hashedLen+8])                 // trailer
	h.Write([]byte{4, 0xff, 0, 0, 0, sigpacket[7] + 6}) // final trailer
	sigsum := h.Sum(nil)
	sig := ed25519.Sign(ctx.key, sigsum)

	// Fill out hash preview
	buf.Write(sigsum[0:2])

	// Fill out signature
	r := sig[:32]
	buf.Write(mpi(r))
	m := sig[32:]
	buf.Write(mpi(m))

	// Finalize
	sigpacket = buf.Bytes()
	sigpacket[1] = byte(len(sigpacket)) - 2
	return sigpacket
}

func newSecretSubkeyPacket(seckey, pubkey []byte, created int64) []byte {
	packet := []byte{
		0xc7, // packet header, new format, Secret-Subkey Packet (7)
		0,    // packet length
		0x04, // packet version, new (4)

		// Public Key
		// creation date
		byte(created >> 24),
		byte(created >> 16),
		byte(created >> 8),
		byte(created >> 0),
		18, // algorithm, Elliptic Curve
		10, // OID length
		// OID (1.3.6.1.4.1.3029.1.5.1)
		0x2b, 0x06, 0x01, 0x04, 0x01, 0x97, 0x55, 0x01, 0x05, 0x01,
		// public key length (always 263 bits)
		0x01, 0x07,
		0x40, // MPI prefix
		// public key (32 bytes)
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		// KDF parameters
		3,    // length
		0x01, // reserved (1)
		0x08, // SHA-256
		0x07, // AES-128? (spec is incorrect)

		// Secret Key
		0, // string-to-key, unencrypted
	}
	copy(packet[22:54], pubkey)

	// append MPI-encoded key
	secmpi := mpi(reverse(seckey))
	packet = append(packet, secmpi...)

	// compute and append checksum
	var checksum uint16
	for _, b := range secmpi {
		checksum += uint16(b)
	}
	packet = append(packet, []byte{
		byte(checksum >> 8), byte(checksum >> 0),
	}...)

	packet[1] = byte(len(packet) - 2)
	return packet
}

// Returns a Signature Packet for an input.
func sign(src io.Reader, keyid []byte, key ed25519.PrivateKey) ([]byte, error) {
	created := time.Now().Unix()
	buf := bytes.NewBuffer([]byte{
		0xc2, // packet header, new format, Signature Packet (2)
		0,    // packet length
		0x04, // packet version, new (4)
		0x00, // signature type, binary document
		22,   // public-key algorithm, EdDSA
		8,    // hash algorithm, SHA-256
		0, 0, // hashed subpacket data length
	})

	// Signature Creation Time subpacket (length=5, type=2)
	buf.Write([]byte{
		5, 2,
		byte(created >> 24),
		byte(created >> 16),
		byte(created >> 8),
		byte(created >> 0),
	})

	// Issuer subpacket (length=9, type=16)
	buf.Write([]byte{
		9, 16,
	})
	buf.Write(keyid[12:])

	// Issuer Fingerprint subpacket (length=22, type=33)
	buf.Write([]byte{
		22, 33,
		04, // fingerprint version
	})
	buf.Write(keyid)

	// Actual hashed subpacket data length
	hashedLen := buf.Len() - 8

	// Unhashed subpacket data (none)
	buf.Write([]byte{
		0, 0,
	})

	// Fill out hashed data length
	sigpacket := buf.Bytes()
	binary.BigEndian.PutUint16(sigpacket[6:], uint16(hashedLen))

	// Compute digest to be signed
	h := sha256.New()
	if _, err := io.Copy(h, src); err != nil {
		return nil, err
	}
	h.Write(sigpacket[2 : hashedLen+8])                 // trailer
	h.Write([]byte{4, 0xff, 0, 0, 0, sigpacket[7] + 6}) // final trailer
	sigsum := h.Sum(nil)
	sig := ed25519.Sign(key, sigsum)

	// Fill out hash preview
	buf.Write(sigsum[0:2])

	// Fill out signature
	r := sig[:32]
	buf.Write(mpi(r))
	m := sig[32:]
	buf.Write(mpi(m))

	// Finalize
	sigpacket = buf.Bytes()
	sigpacket[1] = byte(len(sigpacket)) - 2
	return sigpacket, nil
}

// Modify and return packet with secret key removed.
func stripSecretKeyPacket(skpacket []byte) []byte {
	skpacket[0] = 0xc6
	skpacket[1] = pubKeyLen - 2
	return skpacket[:pubKeyLen]
}

// Modify and return packet with secret subkey removed.
func stripSecretSubkeyPacket(sskpacket []byte) []byte {
	sskpacket[0] = 0xce
	sskpacket[1] = pubSubkeyLen - 2
	return sskpacket[:pubSubkeyLen]
}

// Returns the Key ID from a Secret-Key Packet.
func keyid(skpacket []byte) []byte {
	h := sha1.New()
	h.Write([]byte{0x99, 0, 51})   // "packet" length = 51
	h.Write(skpacket[2:pubKeyLen]) // public key portion
	return h.Sum(nil)
}

// Return the Curve25519 public key for a secret key.
func x25519(seckey []byte) []byte {
	var xpubkey [32]byte
	var xseckey [32]byte
	copy(xseckey[:], seckey)
	curve25519.ScalarBaseMult(&xpubkey, &xseckey)
	return xpubkey[:]
}

// Return a reversed copy.
func reverse(b []byte) []byte {
	c := make([]byte, len(b))
	for i, v := range b {
		c[len(c)-i-1] = v
	}
	return c
}

// Return a Curve25519 keypair from a seed.
func newCurve25519Keys(seed []byte) (seckey, pubkey []byte) {
	seckey = append(seed[:0:0], seed...)
	seckey[0] &= 248
	seckey[31] &= 127
	seckey[31] |= 64
	pubkey = x25519(seckey)
	return
}

func crc24(buf []byte) int32 {
	const (
		crc24Init = 0x0b704ce
		crc24Poly = 0x1864cfb
	)
	var crc int32 = crc24Init
	for _, b := range buf {
		crc ^= int32(b) << 16
		for i := 0; i < 8; i++ {
			crc <<= 1
			if crc&0x1000000 != 0 {
				crc ^= crc24Poly
			}
		}
	}
	return crc & 0xFFFFFF
}

// wrapper is an io.Writer filter that inserts regular hard line breaks.
type wrapper struct {
	w     io.Writer
	max   int
	count int
}

func (w *wrapper) Write(p []byte) (int, error) {
	for len(p) > 0 {
		if w.count == w.max {
			if _, err := w.w.Write([]byte{10}); err != nil {
				return 0, err
			}
			w.count = 0
		}
		left := w.max - w.count
		var line []byte
		if len(p) > left {
			line = p[:left]
		} else {
			line = p
		}
		p = p[len(line):]
		w.count += len(line)
		_, err := w.w.Write(line)
		if err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func main() {
	created := flag.Int64("date", 0, "creation date (unix epoch seconds)")
	fingerprint := flag.Bool("fingerprint", false, "also show fingerprint")
	genSubkey := flag.Bool("subkey", false, "also output an encryption subkey")
	loadKey := flag.String("load", "",
		"load key from file instead of generating")
	now := flag.Bool("now", false, "use current time as creation date")
	paranoid := flag.Bool("paranoid", false, "paranoid mode")
	ppFile := flag.String("passphrase-file", "", "read passphrase from file")
	publicOnly := flag.Bool("public", false, "only output public key")
	repeat := flag.Uint("repeat", 1, "number of repeated passphrase prompts")
	signOnly := flag.Bool("sign", false, "output detached signature for input")
	uid := flag.String("uid", "", "key user ID (required)")
	flag.Parse()

	if *uid == "" && *loadKey == "" {
		fatal("missing User ID (-uid) option")
	}
	if *now {
		*created = time.Now().Unix()
	}

	var key ed25519.PrivateKey
	var seed []byte
	if *loadKey != "" {
		// Load previously-generated key
		f, err := os.Open(*loadKey)
		if err != nil {
			fatal("%s", err)
		}
		var uidbytes []byte
		key, uidbytes, err = load(f)
		*uid = string(uidbytes)
		*genSubkey = false

	} else {
		// Derive a key from the passphrase
		var passphrase []byte
		var err error
		if *ppFile != "" {
			passphrase, err = firstLine(*ppFile)
		} else {
			passphrase, err = readPassphrase(int(*repeat))
		}
		if err != nil {
			fatal("%s", err)
		}
		scale := 1
		if *paranoid {
			scale = 2 // actually 4x difficulty
		}
		seed = kdf(passphrase, []byte(*uid), scale)
		key = ed25519.NewKeyFromSeed(seed[:32])
	}
	seckey := key[:32]
	pubkey := key[32:]

	// Buffer output and perform all writes at once at the end
	var buf bytes.Buffer

	// Secret-Key Packet
	skpacket := newSecretKeyPacket(seckey, pubkey, *created)
	keyid := keyid(skpacket)
	if *fingerprint {
		fmt.Fprintf(os.Stderr, "%X\n", keyid)
	}
	if *signOnly {
		packet, err := sign(os.Stdin, keyid, key)
		if err != nil {
			fatal("%s", err)
		}
		if _, err := os.Stdout.Write(packet); err != nil {
			fatal("%s", err)
		}
		return
	}
	if *publicOnly {
		buf.Write(stripSecretKeyPacket(skpacket))
	} else {
		buf.Write(skpacket)
	}

	// User ID Packet
	idpacket := newUserIDPacket(*uid)
	buf.Write(idpacket)

	// Signature Packet (primary key)
	sigpacket := signKey(&signatureContext{
		key:      key,
		skpacket: skpacket,
		idpacket: idpacket,
		created:  *created,
		mdc:      *genSubkey,
	})
	buf.Write(sigpacket)

	if *genSubkey {
		// Secret-Subkey Packet
		subseckey, subpubkey := newCurve25519Keys(seed[32:])
		sskpacket := newSecretSubkeyPacket(subseckey, subpubkey, *created)
		if *publicOnly {
			buf.Write(stripSecretSubkeyPacket(sskpacket))
		} else {
			buf.Write(sskpacket)
		}

		// Signature Packet (subkey)
		ssigpacket := signKey(&signatureContext{
			key:       key,
			skpacket:  skpacket,
			sskpacket: sskpacket,
			created:   *created,
		})
		buf.Write(ssigpacket)
	}

	output := buf.Bytes()
	if *publicOnly {
		// Wrap in ASCII armor
		var asc bytes.Buffer
		asc.WriteString("-----BEGIN PGP PUBLIC KEY BLOCK-----\n\n")
		wrap := &wrapper{&asc, 78, 0}
		b64 := base64.NewEncoder(base64.RawStdEncoding, wrap)
		b64.Write(output)
		b64.Close()
		asc.WriteString("\n=")
		b64 = base64.NewEncoder(base64.RawStdEncoding, &asc)
		crc := crc24(output)
		b64.Write([]byte{byte(crc >> 16), byte(crc >> 8), byte(crc)})
		b64.Close()
		asc.WriteString("\n-----END PGP PUBLIC KEY BLOCK-----\n")
		output = asc.Bytes()
	}
	if _, err := os.Stdout.Write(output); err != nil {
		fatal("%s", err)
	}
}
