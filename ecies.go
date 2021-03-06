/*
	ecies.go

	Implementation of the EC integrated encryption scheme

	THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
	"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
	LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
	A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
	OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
	SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
	LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
	DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
	THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
	(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
	OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

	ecies.go Daniel Havir, 2018
*/

package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/sha512"
	"hash"
	"io"
	"math/big"

	"golang.org/x/crypto/poly1305"
)

// PublicKey is a structure for storing information relevant to the public key
type PublicKey struct {
	X *big.Int
	Y *big.Int
	elliptic.Curve
}

// PrivateKey is a structure for storing information relevant to the private key
type PrivateKey struct {
	PublicKey
	D *big.Int
}

// GenerateKey is the constructor for Private-Public key pair
func GenerateKey(rand io.Reader, curve elliptic.Curve) *PrivateKey {
	privateBytes, x, y, err := elliptic.GenerateKey(curve, rand)
	check(err)
	public := PublicKey{X: x, Y: y, Curve: curve}
	private := PrivateKey{PublicKey: public, D: new(big.Int).SetBytes(privateBytes)}
	return &private
}

// DeriveShared is method to derive a shared secret
func (private *PrivateKey) DeriveShared(public *PublicKey, keySize int) []byte {
	if private.PublicKey.Curve != public.Curve {
		panic("Curves don't match")
	}
	if 2*keySize > (public.Curve.Params().BitSize+7)/8 {
		panic("Shared key length is too long")
	}

	x, _ := public.Curve.ScalarMult(public.X, public.Y, private.D.Bytes())
	if x == nil {
		panic("Scalar multiplication resulted in infinity")
	}

	shared := x.Bytes()
	return shared
}

// Key-Derivation Function
func kdf(hash hash.Hash, shared, s1 []byte) []byte {
	hash.Write(shared)
	if s1 != nil {
		hash.Write(s1)
	}
	key := hash.Sum(nil)
	hash.Reset()
	return key
}

func sumTag(in, shared []byte, key *[32]byte) [16]byte {
	var out [16]byte
	poly1305.Sum(&out, append(in, shared...), key)
	return out
}

func verifyTag(mac *[16]byte, in, shared []byte, key *[32]byte) bool {
	return poly1305.Verify(mac, append(in, shared...), key)
}

func encryptSymmetric(rand io.Reader, in, key []byte) []byte {
	block, err := aes.NewCipher(key)
	check(err)

	nonce := getCryptoRandVec(rand, aes.BlockSize)
	cipher := cipher.NewCTR(block, nonce)

	out := make([]byte, len(in))
	cipher.XORKeyStream(out, in)

	out = append(nonce, out...)
	return out
}

func decryptSymmetric(in, key []byte) []byte {
	block, err := aes.NewCipher(key)
	check(err)

	cipher := cipher.NewCTR(block, in[:aes.BlockSize])

	out := make([]byte, len(in)-aes.BlockSize)
	cipher.XORKeyStream(out, in[aes.BlockSize:])

	return out
}

// Encrypt is a function for encryption
func Encrypt(rand io.Reader, public *PublicKey, in, s1, s2 []byte) []byte {
	private := GenerateKey(rand, public.Curve)

	curveName := public.Curve.Params().Name
	var hashFunc hash.Hash
	if curveName == "P-521" {
		hashFunc = sha512.New()
	} else {
		hashFunc = sha256.New()
	}
	keySize := hashFunc.Size() / 2

	shared := private.DeriveShared(public, keySize)
	K := kdf(hashFunc, shared, s1)
	Ke := K[:keySize]
	Km := K[keySize:]
	if len(Km) < 32 {
		// Hash K_m so that it's 32 bytes long (required for Poly1305)
		hashFunc.Write(Km)
		Km = hashFunc.Sum(nil)
		hashFunc.Reset()
	}

	c := encryptSymmetric(rand, in, Ke)

	tag := sumTag(c, s2, to32ByteArray(Km))

	R := elliptic.Marshal(public.Curve, private.PublicKey.X, private.PublicKey.Y)
	out := make([]byte, len(R)+len(c)+len(tag))
	copy(out, R)
	copy(out[len(R):], c)
	copy(out[len(R)+len(c):], tag[:])
	return out
}

// Decrypt is a function for decryption
func Decrypt(private *PrivateKey, in, s1, s2 []byte) []byte {
	curveName := private.PublicKey.Curve.Params().Name
	var hashFunc hash.Hash
	if curveName == "P-521" {
		hashFunc = sha512.New()
	} else {
		hashFunc = sha256.New()
	}
	keySize := hashFunc.Size() / 2

	var messageStart int
	macLen := poly1305.TagSize

	if in[0] == 2 || in[0] == 3 || in[0] == 4 {
		messageStart = (private.PublicKey.Curve.Params().BitSize + 7) / 4
		if len(in) < (messageStart + macLen + 1) {
			panic("Invalid message")
		}
	} else {
		panic("Invalid public key")
	}

	if curveName == "P-521" {
		// P-521 curve is serialized into 133 bytes, above formula yields size of only 132, therefore we must add 1
		// P-256 curve is serialized into 65 bytes, above formula yields correct result
		messageStart++
	}

	messageEnd := len(in) - macLen

	R := new(PublicKey)
	R.Curve = private.PublicKey.Curve
	R.X, R.Y = elliptic.Unmarshal(R.Curve, in[:messageStart])
	if R.X == nil {
		panic("Invalid public key. Maybe you didn't specify the right mode?")
	}
	if !R.Curve.IsOnCurve(R.X, R.Y) {
		panic("Invalid curve")
	}

	shared := private.DeriveShared(R, keySize)

	K := kdf(hashFunc, shared, s1)

	Ke := K[:keySize]
	Km := K[keySize:]
	if len(Km) < 32 {
		// Hash K_m so that it's 32 bytes long (required for Poly1305)
		hashFunc.Write(Km)
		Km = hashFunc.Sum(nil)
		hashFunc.Reset()
	}

	match := verifyTag(to16ByteArray(in[messageEnd:]), in[messageStart:messageEnd], s2, to32ByteArray(Km))
	if !match {
		panic("Message tags don't match")
	}

	out := decryptSymmetric(in[messageStart:messageEnd], Ke)
	return out
}
