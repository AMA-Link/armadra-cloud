package server

// stack 是收尾函数栈：先进后出，启动中途失败与正常关闭都从这里按反序收。
type stack struct{ fns []func() }

func (s *stack) push(fn func()) { s.fns = append(s.fns, fn) }

func (s *stack) unwind() {
	for i := len(s.fns) - 1; i >= 0; i-- {
		s.fns[i]()
	}
	s.fns = nil
}
