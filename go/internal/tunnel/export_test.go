package tunnel

// SendCredit 是隧道级发送信用的当前值（只给测试看）。
func (t *Tunnel) SendCredit() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sendCredit
}

// Tails 是已摘表、出队列还没写空的流数（只给测试看）。
func (t *Tunnel) Tails() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tails
}

// CtlQueued 是写端控制帧队列里还没取走的帧数（只给测试看）。
func (t *Tunnel) CtlQueued() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.ctl)
}
