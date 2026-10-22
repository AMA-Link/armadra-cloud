package tlsx

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
)

func encodeSEC1(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}
