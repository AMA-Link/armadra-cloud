package tunnel

// SendCredit 是隧道级发送信用的当前值（只给测试看）。
func (t *Tunnel) SendCredit() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sendCredit
}
