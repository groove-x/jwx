package jwe

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"hash"

	"github.com/lestrrat/go-jwx/jwa"
)

func NewAesKeyWrap(alg jwa.KeyEncryptionAlgorithm, sharedkey []byte) (KeyWrapEncrypt, error) {
	return KeyWrapEncrypt{
		alg:       alg,
		sharedkey: sharedkey,
	}, nil
}

func (kw KeyWrapEncrypt) Algorithm() jwa.KeyEncryptionAlgorithm {
	return kw.alg
}

func (kw KeyWrapEncrypt) Kid() string {
	return kw.KeyID
}

func (kw KeyWrapEncrypt) KeyDecrypt(enckey []byte) ([]byte, error) {
	block, err := aes.NewCipher(kw.sharedkey)
	if err != nil {
		return nil, err
	}

	cek, err := keyunwrap(block, enckey)
	if err != nil {
		return nil, err
	}
	return cek, nil
}

func (kw KeyWrapEncrypt) KeyEncrypt(cek []byte) ([]byte, error) {
	block, err := aes.NewCipher(kw.sharedkey)
	if err != nil {
		println("newcipher failed")
		return nil, err
	}
	encrypted, err := keywrap(block, cek)
	if err != nil {
		return nil, err
	}
	return encrypted, nil
}

func NewRSAKeyEncrypt(alg jwa.KeyEncryptionAlgorithm, pubkey *rsa.PublicKey) *RSAKeyEncrypt {
	return &RSAKeyEncrypt{
		alg:    alg,
		pubkey: pubkey,
	}
}

func (e RSAKeyEncrypt) Algorithm() jwa.KeyEncryptionAlgorithm {
	return e.alg
}

func (e RSAKeyEncrypt) Kid() string {
	return e.KeyID
}

func (e RSAKeyEncrypt) KeyEncrypt(cek []byte) ([]byte, error) {
	debug("RSA.KeyEncrypt: cek = %x", cek)
	if e.alg == jwa.RSA1_5 {
		return rsa.EncryptPKCS1v15(rand.Reader, e.pubkey, cek)
	}

	var hash hash.Hash
	switch e.alg {
	case jwa.RSA_OAEP:
		hash = sha1.New()
	case jwa.RSA_OAEP_256:
		hash = sha256.New()
	default:
		return nil, ErrUnsupportedAlgorithm
	}
	return rsa.EncryptOAEP(hash, rand.Reader, e.pubkey, cek, []byte{})
}

func NewRSAPKCS15KeyDecrypt(alg jwa.KeyEncryptionAlgorithm, privkey *rsa.PrivateKey, keysize int) *RSAPKCS15KeyDecrypt {
	generator := NewRandomKeyGenerate(keysize * 2)
	return &RSAPKCS15KeyDecrypt{
		alg:       alg,
		privkey:   privkey,
		generator: generator,
	}
}

func (d RSAPKCS15KeyDecrypt) Algorithm() jwa.KeyEncryptionAlgorithm {
	return d.alg
}

func (d RSAPKCS15KeyDecrypt) KeyDecrypt(enckey []byte) ([]byte, error) {
	// Hey, these notes and workarounds were stolen from go-jose
	defer func() {
		// DecryptPKCS1v15SessionKey sometimes panics on an invalid payload
		// because of an index out of bounds error, which we want to ignore.
		// This has been fixed in Go 1.3.1 (released 2014/08/13), the recover()
		// only exists for preventing crashes with unpatched versions.
		// See: https://groups.google.com/forum/#!topic/golang-dev/7ihX6Y6kx9k
		// See: https://code.google.com/p/go/source/detail?r=58ee390ff31602edb66af41ed10901ec95904d33
		_ = recover()
	}()

	// Perform some input validation.
	expectedlen := d.privkey.PublicKey.N.BitLen() / 8
	if expectedlen != len(enckey) {
		// Input size is incorrect, the encrypted payload should always match
		// the size of the public modulus (e.g. using a 2048 bit key will
		// produce 256 bytes of output). Reject this since it's invalid input.
		return nil, errors.New("input size for key decrypt is incorrect")
	}

	var err error

	cek, err := d.generator.KeyGenerate()
	if err != nil {
		return nil, errors.New("failed to generate key")
	}

	// When decrypting an RSA-PKCS1v1.5 payload, we must take precautions to
	// prevent chosen-ciphertext attacks as described in RFC 3218, "Preventing
	// the Million Message Attack on Cryptographic Message Syntax". We are
	// therefore deliberatly ignoring errors here.
	debug("OAEP.KeyDecrypt: enckey = %x", enckey)
	err = rsa.DecryptPKCS1v15SessionKey(rand.Reader, d.privkey, enckey, cek)
	if err != nil {
		return nil, err
	}

	debug("OAEP.KeyDecrypt: cek = %x", cek)
	return cek, nil
}

type RSAOAEPKeyDecrypt struct {
	alg     jwa.KeyEncryptionAlgorithm
	privkey *rsa.PrivateKey
}

func NewRSAOAEPKeyDecrypt(alg jwa.KeyEncryptionAlgorithm, privkey *rsa.PrivateKey) *RSAOAEPKeyDecrypt {
	return &RSAOAEPKeyDecrypt{
		alg:     alg,
		privkey: privkey,
	}
}

func (d RSAOAEPKeyDecrypt) Algorithm() jwa.KeyEncryptionAlgorithm {
	return d.alg
}

func (d RSAOAEPKeyDecrypt) KeyDecrypt(enckey []byte) ([]byte, error) {
	var hash hash.Hash
	switch d.alg {
	case jwa.RSA_OAEP:
		hash = sha1.New()
	case jwa.RSA_OAEP_256:
		hash = sha256.New()
	default:
		return nil, ErrUnsupportedAlgorithm
	}
	debug("OAEP.KeyDecrypt: enckey = %x", enckey)
	return rsa.DecryptOAEP(hash, rand.Reader, d.privkey, enckey, []byte{})
}

type DirectDecrypt struct {
	Key []byte
}

func (d DirectDecrypt) Decrypt() ([]byte, error) {
	cek := make([]byte, len(d.Key))
	copy(cek, d.Key)
	return cek, nil
}

var keywrapDefaultIV = []byte{0xa6, 0xa6, 0xa6, 0xa6, 0xa6, 0xa6, 0xa6, 0xa6}

const keywrapChunkLen = 8

func keywrap(kek cipher.Block, cek []byte) ([]byte, error) {
	if len(cek)%8 != 0 {
		return nil, ErrInvalidBlockSize
	}

	n := len(cek) / keywrapChunkLen
	r := make([][]byte, n)

	for i := 0; i < n; i++ {
		r[i] = make([]byte, keywrapChunkLen)
		copy(r[i], cek[i*keywrapChunkLen:])
	}

	buffer := make([]byte, keywrapChunkLen*2)
	tBytes := make([]byte, keywrapChunkLen)
	copy(buffer, keywrapDefaultIV)

	for t := 0; t < 6*n; t++ {
		copy(buffer[keywrapChunkLen:], r[t%n])

		kek.Encrypt(buffer, buffer)

		binary.BigEndian.PutUint64(tBytes, uint64(t+1))

		for i := 0; i < keywrapChunkLen; i++ {
			buffer[i] = buffer[i] ^ tBytes[i]
		}
		copy(r[t%n], buffer[keywrapChunkLen:])
	}

	out := make([]byte, (n+1)*keywrapChunkLen)
	copy(out, buffer[:keywrapChunkLen])
	for i := range r {
		copy(out[(i+1)*8:], r[i])
	}

	return out, nil
}

func keyunwrap(block cipher.Block, ciphertxt []byte) ([]byte, error) {
	if len(ciphertxt)%keywrapChunkLen != 0 {
		return nil, ErrInvalidBlockSize
	}

	n := (len(ciphertxt) / keywrapChunkLen) - 1
	r := make([][]byte, n)

	for i := range r {
		r[i] = make([]byte, keywrapChunkLen)
		copy(r[i], ciphertxt[(i+1)*keywrapChunkLen:])
	}

	buffer := make([]byte, keywrapChunkLen*2)
	tBytes := make([]byte, keywrapChunkLen)
	copy(buffer[:keywrapChunkLen], ciphertxt[:keywrapChunkLen])

	for t := 6*n - 1; t >= 0; t-- {
		binary.BigEndian.PutUint64(tBytes, uint64(t+1))

		for i := 0; i < keywrapChunkLen; i++ {
			buffer[i] = buffer[i] ^ tBytes[i]
		}
		copy(buffer[keywrapChunkLen:], r[t%n])

		block.Decrypt(buffer, buffer)

		copy(r[t%n], buffer[keywrapChunkLen:])
	}

	if subtle.ConstantTimeCompare(buffer[:keywrapChunkLen], keywrapDefaultIV) == 0 {
		return nil, errors.New("keywrap: failed to unwrap key")
	}

	out := make([]byte, n*keywrapChunkLen)
	for i := range r {
		copy(out[i*keywrapChunkLen:], r[i])
	}

	return out, nil
}