package tlsx

import (
	"bytes"
	"crypto/tls"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FileOptions 调文件模式的轮询节奏；零值取缺省（2 秒轮询、500 毫秒防抖）。
type FileOptions struct {
	Poll     time.Duration
	Debounce time.Duration
}

type fileManager struct {
	certPath, keyPath string
	opts              FileOptions
	current           atomic.Pointer[Material]
	// 当前在用的两个文件原文，用来判断是否真的换了。
	certRaw, keyRaw []byte
	baseline        [2]fileSig
}

type fileSig struct {
	info os.FileInfo
	err  bool
}

func sigOf(path string) fileSig {
	info, err := os.Stat(path)
	return fileSig{info: info, err: err != nil}
}

func (a fileSig) same(b fileSig) bool {
	if a.err || b.err {
		return a.err == b.err
	}
	return os.SameFile(a.info, b.info) && a.info.ModTime().Equal(b.info.ModTime()) && a.info.Size() == b.info.Size()
}

// File 是文件模式的 Manager：每 2 秒轮询、500 ms 防抖后热换；新的一对读不出或不匹配时继续用旧的。
func File(certPath, keyPath string) (Manager, error) {
	return FileWith(certPath, keyPath, FileOptions{})
}

// FileWith 同 File，可调轮询与防抖。
func FileWith(certPath, keyPath string, opts FileOptions) (Manager, error) {
	if opts.Poll <= 0 {
		opts.Poll = 2 * time.Second
	}
	if opts.Debounce <= 0 {
		opts.Debounce = 500 * time.Millisecond
	}
	f := &fileManager{certPath: certPath, keyPath: keyPath, opts: opts}
	// 先取基线再读文件：读到一半被替换的话，下一次轮询会发现。
	f.baseline = [2]fileSig{sigOf(certPath), sigOf(keyPath)}
	m, certRaw, keyRaw, err := loadFileMaterial(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	f.certRaw, f.keyRaw = certRaw, keyRaw
	f.current.Store(&m)
	return f, nil
}

func loadFileMaterial(certPath, keyPath string) (Material, []byte, []byte, error) {
	certRaw, err1 := os.ReadFile(certPath)
	keyRaw, err2 := os.ReadFile(keyPath)
	if err1 != nil || err2 != nil {
		return Material{}, nil, nil, tlsErr("证书或私钥读不出来：检查 --tls-cert 与 --tls-key")
	}
	pair, err := tls.X509KeyPair(certRaw, keyRaw)
	if err != nil {
		if strings.Contains(err.Error(), "does not match") {
			return Material{}, nil, nil, tlsErr("证书与私钥对不上")
		}
		return Material{}, nil, nil, tlsErr("证书或私钥读不出来：检查 --tls-cert 与 --tls-key")
	}
	_, leaf, err := buildMaterial(certRaw, keyRaw)
	if err != nil {
		return Material{}, nil, nil, tlsErr("证书或私钥读不出来：检查 --tls-cert 与 --tls-key")
	}
	pair.Leaf = leaf
	blocks := pemCertBlocks(certRaw)
	m := Material{Certificate: pair, NotAfter: leaf.NotAfter, Source: "file", Fingerprint: fingerprintOf(leaf.Raw)}
	if len(blocks) > 1 {
		m.Anchor = blocks[len(blocks)-1]
		top := pair.Certificate[len(pair.Certificate)-1]
		m.Fingerprint = fingerprintOf(top)
	}
	return m, certRaw, keyRaw, nil
}

func (f *fileManager) Current() Material { return *f.current.Load() }

func (f *fileManager) reload(onChange func(Material), onError func(error)) {
	m, certRaw, keyRaw, err := loadFileMaterial(f.certPath, f.keyPath)
	if err != nil {
		// 换到一半（证书新、私钥旧）时对不上：继续用旧的，等下一次变化。
		if onError != nil {
			onError(err)
		}
		return
	}
	if bytes.Equal(certRaw, f.certRaw) && bytes.Equal(keyRaw, f.keyRaw) {
		return
	}
	f.certRaw, f.keyRaw = certRaw, keyRaw
	f.current.Store(&m)
	if onChange != nil {
		onChange(m)
	}
}

func (f *fileManager) Start(onChange func(Material), onError func(error)) func() {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		poll := time.NewTicker(f.opts.Poll)
		defer poll.Stop()
		// 防抖定时器：先停住，检测到变化时重置。
		debounce := time.NewTimer(time.Hour)
		debounce.Stop()
		defer debounce.Stop()
		for {
			select {
			case <-done:
				return
			case <-poll.C:
				now := [2]fileSig{sigOf(f.certPath), sigOf(f.keyPath)}
				if !now[0].same(f.baseline[0]) || !now[1].same(f.baseline[1]) {
					f.baseline = now
					debounce.Reset(f.opts.Debounce)
				}
			case <-debounce.C:
				f.reload(onChange, onError)
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}
