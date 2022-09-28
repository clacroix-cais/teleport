//go:build libpcsclite
// +build libpcsclite

/*
Copyright 2022 Gravitational, Inc.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package keys

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"strings"
	"time"

	"github.com/go-piv/piv-go/piv"
	"github.com/gravitational/trace"

	"github.com/gravitational/teleport/api"
	attestation "github.com/gravitational/teleport/api/gen/proto/go/attestation/v1"
	"github.com/gravitational/teleport/api/utils/retryutils"
)

const (
	// PIVCardTypeYubiKey is the PIV card type assigned to yubiKeys.
	PIVCardTypeYubiKey = "yubikey"
)

var (
	// We use slot 9a for Teleport Clients which require `private_key_policy: hardware_key`.
	pivSlotNoTouch = piv.SlotAuthentication
	// We use slot 9c for Teleport Clients which require `private_key_policy: hardware_key_touch`.
	// Private keys generated on this slot will use TouchPolicy=Cached.
	pivSlotWithTouch = piv.SlotSignature
)

// getYubiKeyPrivateKey connects to a connected yubiKey and gets a private key
// matching the given touch requirement previously generated by a Teleport client.
// If no key exists, a trace.NotFound error will be returned. If the PIV slot is
// in use by a non-Teleport Client, a trace.CompareFailed error will be returned.
func getYubiKeyPrivateKey(ctx context.Context, touchRequired bool) (*PrivateKey, error) {
	// Use the first yubiKey we find.
	y, err := findYubiKey(ctx, 0)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Get the correct PIV slot and Touch policy for the given touch requirement.
	pivSlot := pivSlotNoTouch
	if touchRequired {
		pivSlot = pivSlotWithTouch
	}

	// First, check if there is already a private key set up by a Teleport Client.
	priv, err := y.getPrivateKey(ctx, pivSlot)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	keyPEM, err := priv.keyPEM()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return NewPrivateKey(priv, keyPEM)
}

// generateYubiKeyPrivateKey connects to a connected yubiKey and generates a new
// private key matching the given touch requirement.
func generateYubiKeyPrivateKey(ctx context.Context, touchRequired bool) (*PrivateKey, error) {
	// Use the first yubiKey we find.
	y, err := findYubiKey(ctx, 0)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Get the correct PIV slot and Touch policy for the given touch requirement.
	pivSlot := pivSlotNoTouch
	touchPolicy := piv.TouchPolicyNever
	if touchRequired {
		pivSlot = pivSlotWithTouch
		touchPolicy = piv.TouchPolicyCached
	}

	priv, err := y.generatePrivateKey(ctx, pivSlot, touchPolicy)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	keyPEM, err := priv.keyPEM()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return NewPrivateKey(priv, keyPEM)
}

// YubiKeyPrivateKey is a YubiKey PIV private key. Cryptographical operations open
// a new temporary connection to the PIV card to perform the operation.
type YubiKeyPrivateKey struct {
	// yubiKey is a specific yubiKey PIV module.
	*yubiKey
	pivSlot piv.Slot
	pub     crypto.PublicKey
	// ctx is used when opening a connection to the PIV module,
	// which occurs with a retry loop.
	ctx context.Context
}

// yubiKeyPrivateKeyData is marshalable data used to retrieve a specific yubiKey PIV private key.
type yubiKeyPrivateKeyData struct {
	SerialNumber uint32 `json:"serial_number"`
	SlotKey      uint32 `json:"slot_key"`
}

func newYubiKeyPrivateKey(ctx context.Context, y *yubiKey, slot piv.Slot, pub crypto.PublicKey) (*YubiKeyPrivateKey, error) {
	return &YubiKeyPrivateKey{
		yubiKey: y,
		pivSlot: slot,
		pub:     pub,
		ctx:     ctx,
	}, nil
}

func parseYubiKeyPrivateKeyData(keyDataBytes []byte) (*YubiKeyPrivateKey, error) {
	// TODO (Joerger): rather than requiring a context be passed here, we should
	// pre-load the yubikey PIV connection to avoid retry/context logic occurring
	// at spontaneous points in the code (anywhere a private key is used).
	ctx := context.TODO()

	var keyData yubiKeyPrivateKeyData
	if err := json.Unmarshal(keyDataBytes, &keyData); err != nil {
		return nil, trace.Wrap(err)
	}

	pivSlot, err := parsePIVSlot(keyData.SlotKey)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	y, err := findYubiKey(ctx, keyData.SerialNumber)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	priv, err := y.getPrivateKey(ctx, pivSlot)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return priv, nil
}

// Public returns the public key corresponding to this private key.
func (y *YubiKeyPrivateKey) Public() crypto.PublicKey {
	return y.pub
}

// Sign implements crypto.Signer.
func (y *YubiKeyPrivateKey) Sign(rand io.Reader, digest []byte, opts crypto.SignerOpts) (signature []byte, err error) {
	yk, err := y.open(y.ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer yk.Close()

	privateKey, err := yk.PrivateKey(y.pivSlot, y.pub, piv.KeyAuth{})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return privateKey.(crypto.Signer).Sign(rand, digest, opts)
}

func (y *YubiKeyPrivateKey) keyPEM() ([]byte, error) {
	keyDataBytes, err := json.Marshal(yubiKeyPrivateKeyData{
		SerialNumber: y.serialNumber,
		SlotKey:      y.pivSlot.Key,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return pem.EncodeToMemory(&pem.Block{
		Type:    pivYubiKeyPrivateKeyType,
		Headers: nil,
		Bytes:   keyDataBytes,
	}), nil
}

// GetAttestationStatement returns an AttestationStatement for this YubiKeyPrivateKey.
func (y *YubiKeyPrivateKey) GetAttestationStatement() (*AttestationStatement, error) {
	yk, err := y.open(y.ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer yk.Close()

	slotCert, err := yk.Attest(y.pivSlot)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	attCert, err := yk.AttestationCertificate()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &AttestationStatement{
		AttestationStatement: &attestation.AttestationStatement_YubikeyAttestationStatement{
			YubikeyAttestationStatement: &attestation.YubiKeyAttestationStatement{
				SlotCert:        slotCert.Raw,
				AttestationCert: attCert.Raw,
			},
		},
	}, nil
}

// GetPrivateKeyPolicy returns the PrivateKeyPolicy supported by this YubiKeyPrivateKey.
func (k *YubiKeyPrivateKey) GetPrivateKeyPolicy() PrivateKeyPolicy {
	switch k.pivSlot {
	case pivSlotNoTouch:
		return PrivateKeyPolicyHardwareKey
	case pivSlotWithTouch:
		return PrivateKeyPolicyHardwareKeyTouch
	default:
		return PrivateKeyPolicyNone
	}
}

// yubiKey is a specific yubiKey PIV card.
type yubiKey struct {
	// card is a reader name used to find and connect to this yubiKey.
	// This value may change between OS's, or with other system changes.
	card string
	// serialNumber is the yubiKey's 8 digit serial number.
	serialNumber uint32
}

func newYubiKey(ctx context.Context, card string) (*yubiKey, error) {
	y := &yubiKey{card: card}

	yk, err := y.open(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer yk.Close()

	y.serialNumber, err = yk.Serial()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return y, nil
}

// generatePrivateKey generates a new private key from the given PIV slot with the given PIV policies.
func (y *yubiKey) generatePrivateKey(ctx context.Context, slot piv.Slot, touchPolicy piv.TouchPolicy) (*YubiKeyPrivateKey, error) {
	yk, err := y.open(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer yk.Close()

	opts := piv.Key{
		Algorithm:   piv.AlgorithmEC256,
		PINPolicy:   piv.PINPolicyNever,
		TouchPolicy: touchPolicy,
	}
	pub, err := yk.GenerateKey(piv.DefaultManagementKey, slot, opts)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Create a self signed certificate and store it in the PIV slot so that other
	// Teleport Clients know to reuse the stored key instead of genearting a new one.
	priv, err := yk.PrivateKey(slot, pub, piv.KeyAuth{})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	cert, err := selfSignedTeleportClientCertificate(priv, pub)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Store a self-signed certificate to mark this slot as used by tsh.
	if err = yk.SetCertificate(piv.DefaultManagementKey, slot, cert); err != nil {
		return nil, trace.Wrap(err)
	}

	return newYubiKeyPrivateKey(ctx, y, slot, pub)
}

// getPrivateKey gets an existing private key from the given PIV slot.
func (y *yubiKey) getPrivateKey(ctx context.Context, slot piv.Slot) (*YubiKeyPrivateKey, error) {
	yk, err := y.open(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer yk.Close()

	// Check the slot's certificate to see if it contains a self signed Teleport Client cert.
	cert, err := yk.Certificate(slot)
	if err != nil || cert == nil {
		return nil, trace.NotFound("YubiKey certificate slot is empty, expected a Teleport Client cert")
	} else if len(cert.Subject.Organization) == 0 || cert.Subject.Organization[0] != certOrgName {
		return nil, nonTeleportCertificateError(slot, cert)
	}

	return newYubiKeyPrivateKey(ctx, y, slot, cert.PublicKey)
}

// open a connection to YubiKey PIV module. The returned connection should be closed once
// it's been used. The YubiKey PIV module itself takes some additional time to handle closed
// connections, so we use a retry loop to give the PIV module time to close prior connections.
func (y *yubiKey) open(ctx context.Context) (yk *piv.YubiKey, err error) {
	linearRetry, err := retryutils.NewLinear(retryutils.LinearConfig{
		// If a PIV connection has just been closed, it take ~5-10 ms to become
		// available to new connections. For this reason, we initially wait a
		// short 20ms before stepping up to a longer 100ms retry.
		First: time.Millisecond * 20,
		Step:  time.Millisecond * 100,
		// Since PIV modules only allow a single connection, it is a bottleneck
		// resource. To maximise usage, we use a short 100ms retry to catch the
		// connection opening up as soon as possible.
		Max: time.Millisecond * 100,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Backoff and retry for up to 10 seconds. On login, Teleport Connect tries to open several,
	// maybe even hundreds, of connections to the PIV module all at once to load available resources,
	// so a long retry period is necessary.
	//
	// TODO (joerger): Reduce this retry period to something more reasonable, like 1 second,
	// and add a way for `tsh` and Teleport Connect to share a single connection to a PIV module.
	retryCtx, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()

	err = linearRetry.For(retryCtx, func() error {
		yk, err = piv.Open(y.card)
		if err != nil && !isRetryError(err) {
			return retryutils.PermanentRetryError(err)
		}
		return trace.Wrap(err)
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return yk, nil
}

func isRetryError(err error) bool {
	const retryError = "connecting to smart card: the smart card cannot be accessed because of other connections outstanding"
	return strings.Contains(err.Error(), retryError)
}

// findYubiKey finds a yubiKey PIV card by serial number. If no serial
// number is provided, the first yubiKey found will be returned.
func findYubiKey(ctx context.Context, serialNumber uint32) (*yubiKey, error) {
	yubiKeyCards, err := findYubiKeyCards()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if len(yubiKeyCards) == 0 {
		return nil, trace.NotFound("no yubiKey devices found")
	}

	for _, card := range yubiKeyCards {
		y, err := newYubiKey(ctx, card)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		if serialNumber == 0 || y.serialNumber == serialNumber {
			return y, nil
		}
	}

	return nil, trace.NotFound("no yubiKey device found with serial number %q", serialNumber)
}

// findYubiKeyCards returns a list of connected yubiKey PIV card names.
func findYubiKeyCards() ([]string, error) {
	cards, err := piv.Cards()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	var yubiKeyCards []string
	for _, card := range cards {
		if strings.Contains(strings.ToLower(card), PIVCardTypeYubiKey) {
			yubiKeyCards = append(yubiKeyCards, card)
		}
	}

	return yubiKeyCards, nil
}

func parsePIVSlot(slotKey uint32) (piv.Slot, error) {
	switch slotKey {
	case piv.SlotAuthentication.Key:
		return piv.SlotAuthentication, nil
	case piv.SlotSignature.Key:
		return piv.SlotSignature, nil
	case piv.SlotCardAuthentication.Key:
		return piv.SlotCardAuthentication, nil
	case piv.SlotKeyManagement.Key:
		return piv.SlotKeyManagement, nil
	default:
		retiredSlot, ok := piv.RetiredKeyManagementSlot(slotKey)
		if !ok {
			return piv.Slot{}, trace.BadParameter("slot %X does not exist", slotKey)
		}
		return retiredSlot, nil
	}
}

// certOrgName is used to identify Teleport Client self-signed certificates stored in yubiKey PIV slots.
const certOrgName = "teleport"

func selfSignedTeleportClientCertificate(priv crypto.PrivateKey, pub crypto.PublicKey) (*x509.Certificate, error) {
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit) // see crypto/tls/generate_cert.go
	if err != nil {
		return nil, trace.Wrap(err)
	}
	cert := &x509.Certificate{
		SerialNumber: serialNumber,
		PublicKey:    pub,
		Subject: pkix.Name{
			Organization:       []string{certOrgName},
			OrganizationalUnit: []string{api.Version},
		},
	}
	if cert.Raw, err = x509.CreateCertificate(rand.Reader, cert, cert, pub, priv); err != nil {
		return nil, trace.Wrap(err)
	}
	return cert, nil
}

// nonTeleportCertificateError returns a user-readable CompareFailed error.
// when we run into a non-Teleport certificate, we need to get user input
// before we overwrite it to avoid breaking their non-Teleport PIV programs.
//
// The message is designed to mirror the output of "ykman piv info".
func nonTeleportCertificateError(slot piv.Slot, cert *x509.Certificate) error {
	msg := fmt.Sprintf("YubiKey certificate slot %q contains a non-Teleport certificate:\nSlot %s:\n", slot.String(), slot.String())
	msg += fmt.Sprintf("\tAlgorithm:\t%v\n", cert.SignatureAlgorithm)
	msg += fmt.Sprintf("\tSubject DN:\t%v\n", cert.Subject)
	msg += fmt.Sprintf("\tIssuer DN:\t%v\n", cert.Issuer)
	msg += fmt.Sprintf("\tSerial:\t\t%v\n", cert.SerialNumber)
	msg += fmt.Sprintf("\tFingerprint:\t%v\n", fingerprint(cert))
	msg += fmt.Sprintf("\tNot before:\t%v\n", cert.NotBefore)
	msg += fmt.Sprintf("\tNot after:\t%v\n", cert.NotAfter)
	return trace.CompareFailed(msg)
}

func fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}
