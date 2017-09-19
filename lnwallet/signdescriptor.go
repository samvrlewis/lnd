package lnwallet

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/samvrlewis/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
)

var (
	// ErrTweakOverdose signals a SignDescriptor is invalid because both of its
	// SingleTweak and DoubleTweak are non-nil.
	ErrTweakOverdose = errors.New("sign descriptor should only have one tweak")
)

// SignDescriptor houses the necessary information required to successfully sign
// a given output. This struct is used by the Signer interface in order to gain
// access to critical data needed to generate a valid signature.
type SignDescriptor struct {
	// Pubkey is the public key to which the signature should be generated
	// over. The Signer should then generate a signature with the private
	// key corresponding to this public key.
	PubKey *btcec.PublicKey

	// SingleTweak is a scalar value that will be added to the private key
	// corresponding to the above public key to obtain the private key to
	// be used to sign this input. This value is typically derived via the
	// following computation:
	//
	//  * derivedKey = privkey + sha256(perCommitmentPoint || pubKey) mod N
	//
	// NOTE: If this value is nil, then the input can be signed using only
	// the above public key. Either a SingleTweak should be set or a
	// DoubleTweak, not both.
	SingleTweak []byte

	// DoubleTweak is a private key that will be used in combination with
	// its corresponding private key to derive the private key that is to
	// be used to sign the target input. Within the Lightning protocol,
	// this value is typically the commitment secret from a previously
	// revoked commitment transaction. This value is in combination with
	// two hash values, and the original private key to derive the private
	// key to be used when signing.
	//
	//  * k = (privKey*sha256(pubKey || tweakPub) +
	//        tweakPriv*sha256(tweakPub || pubKey)) mod N
	//
	// NOTE: If this value is nil, then the input can be signed using only
	// the above public key. Either a SingleTweak should be set or a
	// DoubleTweak, not both.
	DoubleTweak *btcec.PrivateKey

	// WitnessScript is the full script required to properly redeem the
	// output. This field will only be populated if a p2wsh or a p2sh
	// output is being signed.
	WitnessScript []byte

	// Output is the target output which should be signed. The PkScript and
	// Value fields within the output should be properly populated,
	// otherwise an invalid signature may be generated.
	Output *wire.TxOut

	// HashType is the target sighash type that should be used when
	// generating the final sighash, and signature.
	HashType txscript.SigHashType

	// SigHashes is the pre-computed sighash midstate to be used when
	// generating the final sighash for signing.
	SigHashes *txscript.TxSigHashes

	// InputIndex is the target input within the transaction that should be
	// signed.
	InputIndex int
}

// WriteSignDescriptor serializes a SignDescriptor struct into the passed
// io.Writer stream.
// NOTE: We assume the SigHashes and InputIndex fields haven't been assigned
// yet, since that is usually done just before broadcast by the witness
// generator.
func WriteSignDescriptor(w io.Writer, sd *SignDescriptor) error {
	serializedPubKey := sd.PubKey.SerializeCompressed()
	if err := wire.WriteVarBytes(w, 0, serializedPubKey); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(w, 0, sd.SingleTweak); err != nil {
		return err
	}

	var doubleTweakBytes []byte
	if sd.DoubleTweak != nil {
		doubleTweakBytes = sd.DoubleTweak.Serialize()
	}
	if err := wire.WriteVarBytes(w, 0, doubleTweakBytes); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(w, 0, sd.WitnessScript); err != nil {
		return err
	}

	if err := lnwire.WriteTxOut(w, sd.Output); err != nil {
		return err
	}

	var scratch [4]byte
	binary.BigEndian.PutUint32(scratch[:], uint32(sd.HashType))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	return nil
}

// ReadSignDescriptor deserializes a SignDescriptor struct from the passed
// io.Reader stream.
func ReadSignDescriptor(r io.Reader, sd *SignDescriptor) error {

	pubKeyBytes, err := wire.ReadVarBytes(r, 0, 34, "pubkey")
	if err != nil {
		return err
	}
	sd.PubKey, err = btcec.ParsePubKey(pubKeyBytes, btcec.S256())
	if err != nil {
		return err
	}

	singleTweak, err := wire.ReadVarBytes(r, 0, 32, "singleTweak")
	if err != nil {
		return err
	}

	// Serializing a SignDescriptor with a nil-valued SingleTweak results in
	// deserializing a zero-length slice. Since a nil-valued SingleTweak has
	// special meaning and a zero-length slice for a SingleTweak is invalid,
	// we can use the zero-length slice as the flag for a nil-valued
	// SingleTweak.
	if len(singleTweak) == 0 {
		sd.SingleTweak = nil
	} else {
		sd.SingleTweak = singleTweak
	}

	doubleTweakBytes, err := wire.ReadVarBytes(r, 0, 32, "doubleTweak")
	if err != nil {
		return err
	}

	// Serializing a SignDescriptor with a nil-valued DoubleTweak results in
	// deserializing a zero-length slice. Since a nil-valued DoubleTweak has
	// special meaning and a zero-length slice for a DoubleTweak is invalid,
	// we can use the zero-length slice as the flag for a nil-valued
	// DoubleTweak.
	if len(doubleTweakBytes) == 0 {
		sd.DoubleTweak = nil
	} else {
		sd.DoubleTweak, _ = btcec.PrivKeyFromBytes(btcec.S256(), doubleTweakBytes)
	}

	// Only one tweak should ever be set, fail if both are present.
	if sd.SingleTweak != nil && sd.DoubleTweak != nil {
		return ErrTweakOverdose
	}

	witnessScript, err := wire.ReadVarBytes(r, 0, 100, "witnessScript")
	if err != nil {
		return err
	}
	sd.WitnessScript = witnessScript

	txOut := &wire.TxOut{}
	if err := lnwire.ReadTxOut(r, txOut); err != nil {
		return err
	}
	sd.Output = txOut

	var hashType [4]byte
	if _, err := io.ReadFull(r, hashType[:]); err != nil {
		return err
	}
	sd.HashType = txscript.SigHashType(binary.BigEndian.Uint32(hashType[:]))

	return nil
}
