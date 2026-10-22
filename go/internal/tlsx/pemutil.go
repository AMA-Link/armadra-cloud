package tlsx

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// fingerprintOf 是证书 DER 的 SHA-256，小写十六进制、无分隔符。
func fingerprintOf(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// pemCertBlocks 取出文本里所有证书块（原样重新编码，每块以换行结尾）。
func pemCertBlocks(data []byte) [][]byte {
	var out [][]byte
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return out
		}
		if block.Type == "CERTIFICATE" {
			out = append(out, pem.EncodeToMemory(block))
		}
	}
}

func certPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// keyPEM 把 EC 私钥编成 PKCS8（与 TS 版落盘格式一致）。
func keyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// parseECKey 读 PKCS8 或 SEC1 的 EC 私钥 PEM。
func parseECKey(data []byte) (*ecdsa.PrivateKey, error) {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("没有私钥块")
		}
		switch block.Type {
		case "PRIVATE KEY":
			key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, err
			}
			ec, ok := key.(*ecdsa.PrivateKey)
			if !ok {
				return nil, fmt.Errorf("不是 EC 私钥")
			}
			return ec, nil
		case "EC PRIVATE KEY":
			return x509.ParseECPrivateKey(block.Bytes)
		}
	}
}

// parseCert 读 PEM 里的第一张证书。
func parseCert(data []byte) (*x509.Certificate, error) {
	blocks := pemCertBlocks(data)
	if len(blocks) == 0 {
		return nil, fmt.Errorf("没有证书块")
	}
	block, _ := pem.Decode(blocks[0])
	return x509.ParseCertificate(block.Bytes)
}

// buildMaterial 把证书链与私钥 PEM 装成 tls.Certificate；对不上时答错误。
func buildMaterial(chainPEM, key []byte) (tls.Certificate, *x509.Certificate, error) {
	pair, err := tls.X509KeyPair(chainPEM, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pair.Leaf = leaf
	return pair, leaf, nil
}

// replaceFile 先写同目录的临时文件再改名；mode 在写之前就定好，不受 umask 放宽。
func replaceFile(file string, content []byte, mode os.FileMode) error {
	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	tmp := fmt.Sprintf("%s.%d.%s.tmp", file, os.Getpid(), hex.EncodeToString(suffix[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil {
		err = os.Rename(tmp, file)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// covers 判断证书是否覆盖所有名字（DNS 不区分大小写，IP 按地址比较）。
func covers(c *x509.Certificate, names []string) bool {
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			found := false
			for _, have := range c.IPAddresses {
				if have.Equal(ip) {
					found = true
				}
			}
			if !found {
				return false
			}
			continue
		}
		found := false
		for _, have := range c.DNSNames {
			if strings.EqualFold(have, name) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func joinDir(dataDir, sub string) string { return filepath.Join(dataDir, sub) }
